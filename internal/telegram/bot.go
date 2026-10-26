// Package telegram is the chat front end.
//
// It is a long-polling bot rather than a webhook receiver, because the service
// is self-hosted behind a home connection: a webhook would need a public
// hostname, a certificate and a port forward, and long polling needs none of
// them.
//
// Nothing here is on the critical path. The bot starts and stops workflows and
// reads the projection, exactly like the CLI and the web interface; the work
// itself happens in the worker. A bot that is switched off means no
// notifications reach Telegram and no requests can be started from a chat —
// and the notification rows wait in the database until it comes back.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/fx"
	xproxy "golang.org/x/net/proxy"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/request"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/tmdb"
)

// Module wires the bot into the fx graph.
var Module = fx.Module("telegram",
	fx.Provide(New),
	fx.Invoke(registerBot),
)

// mediaLookup is the part of the metadata client this package uses.
//
// Declared here rather than taken as *tmdb.Client for one reason: the search
// flow is the bot's main path, and a concrete client would leave it testable
// only against the real TMDB. A three-method interface is the whole cost.
type mediaLookup interface {
	SearchMulti(ctx context.Context, query string) ([]tmdb.SearchResult, error)
	Movie(ctx context.Context, tmdbID int64) (*tmdb.Title, error)
	TV(ctx context.Context, tmdbID int64) (*tmdb.Title, error)
}

// requestStarter is the part of the workflow service this package uses.
//
// Two methods of a type that has five. Narrowing it is what lets the handlers
// be tested without a Temporal server, and it also documents what the bot can
// do: it starts requests and cancels them, and it cannot touch a running
// workflow in any other way.
type requestStarter interface {
	StartRequest(ctx context.Context, in request.StartInput) (*request.StartResult, error)
	Cancel(ctx context.Context, requestID int64) error
	SignalPick(ctx context.Context, requestID int64) error
}

// Bot answers messages and delivers notifications.
type Bot struct {
	cfg   *config.Config
	api   *tgbot.Bot
	store *storage.Store
	req   requestStarter
	tmdb  mediaLookup
	log   *slog.Logger
}

// Options override the collaborators, for tests.
type Options struct {
	API  *tgbot.Bot
	TMDB mediaLookup
	Req  requestStarter
}

// New builds the bot, or a switched-off one when there is nothing to poll
// with.
//
// A disabled bot is a real value with a nil client rather than a nil *Bot, so
// that fx has something to inject and the lifecycle hook decides what to do
// with it. Keeping that decision in one place is what lets `mediad all` run
// with no Telegram configuration at all.
func New(cfg *config.Config, store *storage.Store, req *request.Service,
	client *tmdb.Client, log *slog.Logger) (*Bot, error) {
	return newBot(cfg, store, req, client, log, Options{})
}

func newBot(cfg *config.Config, store *storage.Store, req requestStarter,
	client mediaLookup, log *slog.Logger, opts Options) (*Bot, error) {
	log = log.With("component", "telegram")

	b := &Bot{cfg: cfg, store: store, req: req, tmdb: client, log: log}

	// An unset token means the bot is not configured, which is not the same as
	// misconfigured. It is the ordinary case for somebody who wants the
	// interface and the worker and nothing else, so it is quiet rather than
	// fatal — including under `mediad all`, where a startup failure over an
	// optional feature would take down the parts that were configured.
	//
	// `mediad bot` is where that changes: a command whose entire job is the
	// bot refuses to start without one, and says what to set.
	if !cfg.Telegram.Enabled {
		log.Info("the telegram bot is disabled")
		return b, nil
	}
	if strings.TrimSpace(cfg.Telegram.Token) == "" {
		log.Info("the telegram bot is idle: telegram.token is not set")
		return b, nil
	}

	// The routes, as options rather than as calls after construction, so that
	// both paths below register exactly the same set.
	//
	// This is not tidiness. Given an already-built client — which is what a
	// test does, pointed at a stub server — a second registration path that
	// forgot one of these would leave that command falling through to the
	// default handler, where free text is treated as a search. "/lst" would
	// then be searched for as a film called "/lst", and nothing would say so.
	// The command names are given without their slash, which is how the
	// library matches them: it takes the bot_command entity Telegram attaches,
	// slices off the leading character and compares the rest. Registering
	// "/list" therefore matches nothing at all — and the consequence is not a
	// missing command but a wrong one, because an unmatched message falls
	// through to the default handler, which treats free text as a search. A
	// user typing /list would get a TMDB search for a film called "/list".
	//
	// Matching is on the entity rather than on the text for the same reason
	// "/list@my_bot" arrives in groups: the slash alone is not what makes a
	// command.
	routes := []tgbot.Option{
		tgbot.WithDefaultHandler(b.handleDefault),
		tgbot.WithMessageTextHandler("start", tgbot.MatchTypeCommand, b.handleStart),
		tgbot.WithMessageTextHandler("help", tgbot.MatchTypeCommand, b.handleHelp),
		tgbot.WithMessageTextHandler("link", tgbot.MatchTypeCommand, b.handleLink),
		tgbot.WithMessageTextHandler("list", tgbot.MatchTypeCommand, b.handleList),
		tgbot.WithMessageTextHandler("cancel", tgbot.MatchTypeCommand, b.handleCancel),
		tgbot.WithMessageTextHandler("search", tgbot.MatchTypeCommand, b.handleSearch),
		tgbot.WithMessageTextHandler("pick", tgbot.MatchTypeCommand, b.handlePick),
		tgbot.WithCallbackQueryDataHandler(callbackPrefix, tgbot.MatchTypePrefix, b.handleSelect),
		tgbot.WithErrorsHandler(b.handleError),
	}

	if opts.API != nil {
		for _, apply := range routes {
			apply(opts.API)
		}
		b.api = opts.API
		return b, nil
	}

	httpc, err := telegramHTTPClient(cfg.Telegram.Proxy)
	if err != nil {
		return nil, err
	}
	all := append(routes, tgbot.WithHTTPClient(time.Minute, httpc))

	api, err := tgbot.New(cfg.Telegram.Token, all...)
	if err != nil {
		return nil, fmt.Errorf("start the telegram bot: %w", err)
	}

	b.api = api

	// The library calls getMe itself during New; we need the result for the
	// web UI's link-instruction text, and the library throws it away. The
	// second call is a single short HTTP round-trip; not worth a fork in
	// the library. A pre-set BotUsername in config wins, so a deployment
	// behind a firewalled api.telegram.org can still tell users which bot
	// to message.
	if cfg.Telegram.BotUsername == "" {
		gctx, gcancel := context.WithTimeout(context.Background(), 3*time.Second)
		me, err := b.api.GetMe(gctx)
		gcancel()
		if err != nil {
			log.Warn("could not fetch the bot's username via getMe; the link-instruction will not show a handle", "error", err)
		} else {
			cfg.Telegram.BotUsername = me.Username
		}
	}

	return b, nil
}

// registerBot ties polling and delivery to the application lifecycle.
func registerBot(lc fx.Lifecycle, b *Bot) {
	if b == nil || b.api == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			// tgbot.Bot.Start blocks until the polling goroutines exit
			// (i.e. ctx is canceled), so calling it directly would freeze fx
			// app.Start past its 90s timeout and the process would die with
			// "context deadline exceeded" before any user message could be
			// received. Run it in the background; OnStop's cancel brings the
			// goroutines back.
			go b.api.Start(ctx)
			b.log.Info("telegram bot polling")
			go b.deliver(ctx)
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			return nil
		},
	})
}

// handleError logs what the library could not deliver.
//
// Without this the library swallows its own failures, and a bot that answers
// nothing looks like a bot that is not running.
func (b *Bot) handleError(err error) {
	b.log.Warn("telegram reported an error", "error", err)
}

// helpText is the message /help and /start reply.
//
// The web URL is interpolated from the configured BaseURL so a user with
// the bot can be told exactly where to grab an invite code rather than
// fishing for a link.
func (b *Bot) helpText() string {
	web := strings.TrimRight(b.cfg.Server.BaseURL, "/")
	return fmt.Sprintf(`Что я умею.

Пришлите название фильма или сериала — найду на TMDB и покажу варианты.
Дальше: проверю Plex и, если этого там нет, буду искать релиз с русским
звуком в хорошем качестве до двух лет.

/list — что сейчас качается
/cancel <номер> — отменить запрос
/link <код> — привязать этот чат к аккаунту
/help — эта справка

Код для привязки выдаётся в веб-интерфейсе, в настройках: %s/settings?tab=telegram
Без него можно пользоваться ботом, но уведомления придут только сюда, а
не в общий аккаунт.`, web)
}

func (b *Bot) handleStart(ctx context.Context, api *tgbot.Bot, update *models.Update) {
	b.reply(ctx, update, "Привет. Пришлите название фильма или сериала — найду и поставлю запрос.\n\n"+b.helpText())
}

func (b *Bot) handleHelp(ctx context.Context, api *tgbot.Bot, update *models.Update) {
	b.reply(ctx, update, b.helpText())
}

// handleDefault handles free text: anything that is not a command is a search.
func (b *Bot) handleDefault(ctx context.Context, api *tgbot.Bot, update *models.Update) {
	if update.Message == nil || strings.TrimSpace(update.Message.Text) == "" {
		return
	}

	text := strings.TrimSpace(update.Message.Text)
	if strings.HasPrefix(text, "/") {
		// A command nobody registered. Saying so beats treating "/lst" as a
		// search for a film called "/lst".
		b.reply(ctx, update, "Не знаю такой команды. /help — что я умею.")
		return
	}

	b.search(ctx, update, text)
}

func (b *Bot) handleSearch(ctx context.Context, api *tgbot.Bot, update *models.Update) {
	query := commandArgs(update)
	if query == "" {
		b.reply(ctx, update, "Что искать? Например: /search Матрица")
		return
	}
	b.search(ctx, update, query)
}

// handleLink attaches this chat to an existing account.
func (b *Bot) handleLink(ctx context.Context, api *tgbot.Bot, update *models.Update) {
	code := commandArgs(update)
	if code == "" {
		b.reply(ctx, update, "Пришлите код: /link <код>. Код выдаётся в веб-интерфейсе, в настройках.")
		return
	}

	from := update.Message.From
	if from == nil {
		return
	}
	telegramID := telegramIDString(from.ID)

	user, err := b.store.LinkTelegramByCode(ctx, code, telegramID, displayName(*from))
	if err != nil {
		b.log.Info("failed to link a chat", "telegram_id", telegramID, "error", err)
		b.reply(ctx, update, "Не получилось: "+err.Error())
		return
	}

	b.log.Info("telegram chat linked", "user", user.Username, "telegram_id", telegramID)
	b.reply(ctx, update, fmt.Sprintf(
		"Готово. Этот чат привязан к аккаунту «%s», и уведомления будут приходить сюда же, "+
			"где бы запрос ни был создан.", user.Username))
}

// handleList shows the active requests, or the candidate list of one
// request when the command carries a number.
//
// "/list" alone is the overview. "/list 12" is the list of releases the
// request 12 is offering, with the rank each of them has — the number
// "/pick 12 <rank>" takes. The two belong together: the pick command
// points at this one.
func (b *Bot) handleList(ctx context.Context, api *tgbot.Bot, update *models.Update) {
	user, err := b.user(ctx, update)
	if err != nil {
		b.reply(ctx, update, "Сначала привяжите чат: /link <код>. Код есть в веб-интерфейсе.")
		return
	}

	if args := strings.Fields(commandArgs(update)); len(args) > 0 {
		id, convErr := strconv.Atoi(args[0])
		if convErr != nil || id <= 0 {
			b.reply(ctx, update, "Номер должен быть числом. Список запросов — в /list.")
			return
		}
		b.listCandidates(ctx, update, user, int64(id))
		return
	}

	requests, err := b.store.ListRequests(ctx, storage.RequestFilter{
		States:        storage.ActiveStates(),
		WatcherUserID: user.ID,
		Limit:         30,
	})
	if err != nil {
		b.log.Error("could not list requests", "error", err)
		b.reply(ctx, update, "Не получилось посмотреть список.")
		return
	}

	if len(requests) == 0 {
		b.reply(ctx, update, "Ничего не ищется. Пришлите название фильма или сериала.")
		return
	}

	var out strings.Builder
	out.WriteString("Сейчас:\n\n")
	for _, r := range requests {
		fmt.Fprintf(&out, "%d. %s\n   %s\n", r.ID, titleWithSeason(r), stateLine(r))
	}
	out.WriteString("\n/cancel <номер> — отменить")
	out.WriteString("\n/list <номер> — кандидаты на выбор")

	b.reply(ctx, update, out.String())
}

// listCandidates shows the releases a request is offering.
//
// The same defensive check as handlePick: only somebody watching the
// request may see what it is offering, because the list is the picker's
// menu and a pick starts a download.
func (b *Bot) listCandidates(ctx context.Context, update *models.Update, user *storage.User, id int64) {
	watchers, err := b.store.ListWatchers(ctx, id)
	if err != nil {
		b.log.Error("could not list watchers", "request", id, "error", err)
		b.reply(ctx, update, "Не получилось проверить права на этот запрос.")
		return
	}
	if !watching(watchers, user.ID) {
		b.reply(ctx, update, "Это не ваш запрос.")
		return
	}

	candidates, err := b.store.ListCandidates(ctx, id)
	if err != nil {
		b.log.Error("could not list candidates", "request", id, "error", err)
		b.reply(ctx, update, "Не получилось посмотреть кандидатов.")
		return
	}
	if len(candidates) == 0 {
		b.reply(ctx, update, "У этого запроса нет кандидатов на выбор.")
		return
	}

	var out strings.Builder
	out.WriteString("Кандидаты:\n\n")
	for _, c := range candidates {
		fmt.Fprintf(&out, "%d. [%s] %s\n   сидеров: %d  размер: %s\n   отклонён: %s\n",
			c.Rank, c.Indexer, c.Title, c.Seeders, humanBytes(c.SizeBytes), c.RejectionReason)
	}
	out.WriteString("\n/pick <номер> <rank> — скачать этот")

	b.reply(ctx, update, out.String())
}

// humanBytes formats a byte count for a message.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), []string{"КБ", "МБ", "ГБ", "ТБ"}[exp])
}

// handlePick marks a candidate as the user's choice.
//
// Called from a chat linked to the request's owner. The workflow
// polls request_candidates.picked_at on its next tick and treats the
// first row with a non-null timestamp as the choice.
//
// The shape mirrors handleCancel: the chat must be linked to the
// request's owner, the request must exist, the rank must parse, and
// the candidate at that rank must still be on the list. A wrong
// rank returns ErrNoSuchCandidate; a deleted or never-existed list
// is the same answer from the user's perspective.
func (b *Bot) handlePick(ctx context.Context, api *tgbot.Bot, update *models.Update) {
	user, err := b.user(ctx, update)
	if err != nil {
		b.reply(ctx, update, "Сначала привяжите чат: /link <код>.")
		return
	}

	args := strings.Fields(commandArgs(update))
	if len(args) < 2 {
		b.reply(ctx, update, "Формат: /pick <номер> <rank>. Список кандидатов — в /list <номер>.")
		return
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.reply(ctx, update, "Номер должен быть числом. Посмотреть можно в /list.")
		return
	}
	rank, err := strconv.Atoi(args[1])
	if err != nil || rank <= 0 {
		b.reply(ctx, update, "Rank должен быть положительным числом.")
		return
	}

	// Only somebody watching the request may pick from its candidate list.
	// Same defensive check as handleCancel: the id is a small number,
	// and without this any chat could /pick 12 rank 1 and start a
	// download on a stranger's request.
	watchers, err := b.store.ListWatchers(ctx, id)
	if err != nil {
		b.log.Error("could not list watchers", "request", id, "error", err)
		b.reply(ctx, update, "Не получилось проверить права на этот запрос.")
		return
	}
	if !watching(watchers, user.ID) {
		b.reply(ctx, update, "Это не ваш запрос.")
		return
	}

	if _, err := b.store.MarkPicked(ctx, id, rank); err != nil {
		if errors.Is(err, storage.ErrNoSuchCandidate) {
			b.reply(ctx, update, "Такого кандидата нет (rank="+strconv.Itoa(rank)+").")
			return
		}
		b.log.Warn("could not mark candidate picked", "request", id, "rank", rank, "error", err)
		b.reply(ctx, update, "Не получилось: "+err.Error())
		return
	}

	b.log.Info("user picked a candidate via telegram",
		"user", user.Username, "request", id, "rank", rank)

	// Wake the request so the choice takes effect now rather than at the
	// request's next scheduled tick. The choice is already in the
	// database, so a signal that does not arrive costs time, not the
	// choice.
	if err := b.req.SignalPick(ctx, id); err != nil {
		b.log.Warn("could not wake the workflow for a picked candidate",
			"request", id, "rank", rank, "error", err)
		b.reply(ctx, update, fmt.Sprintf("Выбран rank %d. Workflow подхватит на ближайшем тике.", rank))
		return
	}
	b.reply(ctx, update, fmt.Sprintf("Выбран rank %d. Загрузка начнётся через несколько секунд.", rank))
}

// handleCancel stops a request.
func (b *Bot) handleCancel(ctx context.Context, api *tgbot.Bot, update *models.Update) {
	user, err := b.user(ctx, update)
	if err != nil {
		b.reply(ctx, update, "Сначала привяжите чат: /link <код>.")
		return
	}

	arg := commandArgs(update)
	if arg == "" {
		b.reply(ctx, update, "Какой номер? Посмотреть можно в /list.")
		return
	}

	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		b.reply(ctx, update, "Номер должен быть числом. Посмотреть можно в /list.")
		return
	}

	req, err := b.store.GetRequest(ctx, id)
	if err != nil {
		b.reply(ctx, update, "Не нашёл такой запрос.")
		return
	}

	// Only somebody watching it may cancel it: the id is a small number, and
	// without this anybody could type /cancel 12 and stop a stranger's
	// download.
	watchers, err := b.store.ListWatchers(ctx, id)
	if err != nil {
		b.log.Error("could not list watchers", "request", id, "error", err)
		b.reply(ctx, update, "Не получилось проверить права на этот запрос.")
		return
	}
	if !watching(watchers, user.ID) {
		b.reply(ctx, update, "Это не ваш запрос.")
		return
	}

	if err := b.req.Cancel(ctx, id); err != nil {
		b.log.Error("could not cancel", "request", id, "error", err)
		b.reply(ctx, update, "Не получилось отменить: "+err.Error())
		return
	}

	b.reply(ctx, update, fmt.Sprintf(
		"Отменяю «%s». Если торрент уже добавлен, он будет удалён — это займёт момент.",
		titleWithSeason(req)))
}

// ---------------------------------------------------------------------------
// Search and selection
// ---------------------------------------------------------------------------

// handleSelect handles a button press.
func (b *Bot) handleSelect(ctx context.Context, api *tgbot.Bot, update *models.Update) {
	query := update.CallbackQuery
	if query == nil {
		return
	}

	// Answered immediately, always. Telegram spins the button until this call
	// arrives and gives up after a few seconds, and a search that takes longer
	// than that would leave the user looking at a spinner.
	if _, err := b.api.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{
		CallbackQueryID: query.ID,
	}); err != nil {
		b.log.Debug("could not answer a callback", "error", err)
	}

	telegramID := telegramIDString(query.From.ID)
	user, err := b.store.UserByTelegram(ctx, telegramID)
	if err != nil {
		b.send(ctx, query.Message.Message.Chat.ID,
			"Сначала привяжите чат: /link <код>. Код есть в веб-интерфейсе.")
		return
	}

	// sel:<session>:<index>
	parts := strings.Split(query.Data, ":")
	if len(parts) != 3 {
		b.log.Warn("malformed callback data", "data", query.Data)
		return
	}
	sessionID, indexRaw := parts[1], parts[2]

	index, err := strconv.Atoi(indexRaw)
	if err != nil {
		b.log.Warn("malformed callback index", "data", query.Data)
		return
	}

	chatID := query.Message.Message.Chat.ID
	b.choose(ctx, chatID, user, sessionID, index, query.Message.Message.ID)
}

// search runs a TMDB lookup and offers the results.
func (b *Bot) search(ctx context.Context, update *models.Update, query string) {
	user, err := b.user(ctx, update)
	if err != nil {
		b.reply(ctx, update, "Сначала привяжите чат: /link <код>. Код есть в веб-интерфейсе.")
		return
	}

	results, err := b.tmdb.SearchMulti(ctx, query)
	if err != nil {
		b.log.Error("tmdb search failed", "error", err, "query", query)
		b.reply(ctx, update, "Не получилось спросить TMDB: "+err.Error())
		return
	}

	if len(results) == 0 {
		b.reply(ctx, update, fmt.Sprintf("Ничего не нашлось по запросу «%s».", query))
		return
	}

	// The results go into a session and the buttons carry only its id.
	// Telegram caps callback_data at 64 bytes, and a handful of titles in
	// Russian is past that before the first page is full.
	sessionID, err := b.store.SaveSearchSession(ctx, user.ID, storage.SessionResults, results)
	if err != nil {
		b.log.Error("could not save a search session", "error", err)
		b.reply(ctx, update, "Не получилось сохранить результаты поиска.")
		return
	}

	// The album of all result posters above the caption gives the user every
	// thumbnail at a glance; the buttons below it carry the choice. The album
	// is decorative — the button message is what handleSelect gets a message_id
	// back for and edits on press.
	if msg := messageOf(update); msg != nil {
		b.sendOptionsAlbum(ctx, msg.Chat.ID, results,
			fmt.Sprintf("Нашлось: %d. Что из этого?", len(results)),
			resultsKeyboard(sessionID, results))
	}
}

// choose acts on a pressed button.
func (b *Bot) choose(ctx context.Context, chatID int64, user *storage.User,
	sessionID string, index int, messageID int) {
	// The kind is read first, without decoding the payload, and that order
	// matters: both kinds of choice come back as the same button shape, so the
	// stored kind is the only thing that says what the index means. Decoding
	// first and inferring the kind from which decode succeeded would work by
	// accident — an array does not unmarshal into a struct — and would break
	// the day two payloads happen to have compatible shapes.
	session, err := b.store.GetSearchSession(ctx, sessionID, user.ID, nil)
	if err != nil {
		b.explainSessionError(ctx, chatID, err)
		return
	}

	if session.Kind == storage.SessionSeasons {
		var choice seasonChoice
		if err := json.Unmarshal(session.Payload, &choice); err != nil {
			b.log.Error("could not decode a seasons session", "session", sessionID, "error", err)
			b.send(ctx, chatID, "Не получилось разобрать список сезонов.")
			return
		}
		b.chooseSeason(ctx, chatID, user, choice, index, messageID)
		return
	}

	if session.Kind != storage.SessionResults {
		b.log.Warn("a session was used for an unknown kind of choice",
			"kind", session.Kind, "session", sessionID)
		return
	}

	var results []tmdb.SearchResult
	if err := json.Unmarshal(session.Payload, &results); err != nil {
		b.log.Error("could not decode a results session", "session", sessionID, "error", err)
		b.send(ctx, chatID, "Не получилось разобрать результаты поиска.")
		return
	}

	if index < 0 || index >= len(results) {
		b.log.Warn("a callback index was out of range", "index", index, "session", sessionID)
		return
	}

	chosen := results[index]

	// A film is ready to request. A series needs a season, so the same message
	// is reused for the second question rather than piling up replies.
	if chosen.Ref.Type == domain.MediaTypeMovie {
		b.startRequest(ctx, chatID, user, chosen.Ref, messageID)
		return
	}

	show, err := b.tmdb.TV(ctx, chosen.Ref.TMDBID)
	if err != nil {
		b.log.Error("could not load a series", "tmdb", chosen.Ref.TMDBID, "error", err)
		b.send(ctx, chatID, "Не получилось загрузить список сезонов.")
		return
	}
	if len(show.Seasons) == 0 {
		b.send(ctx, chatID, "У этого сериала на TMDB нет сезонов.")
		return
	}

	seasonSession, err := b.store.SaveSearchSession(ctx, user.ID, storage.SessionSeasons,
		seasonChoice{SeriesID: show.Ref.TMDBID, Title: show.Title, Seasons: show.Seasons})
	if err != nil {
		b.log.Error("could not save a seasons session", "error", err)
		b.send(ctx, chatID, "Не получилось сохранить список сезонов.")
		return
	}

	b.editMarkup(ctx, chatID, messageID,
		fmt.Sprintf("%s — какой сезон?", show.Title),
		seasonsKeyboard(seasonSession, show.Seasons))
}

// chooseSeason starts a request for one season.
func (b *Bot) chooseSeason(ctx context.Context, chatID int64, user *storage.User,
	choice seasonChoice, index int, messageID int) {
	if index < 0 || index >= len(choice.Seasons) {
		b.log.Warn("a season index was out of range", "index", index)
		return
	}

	season := choice.Seasons[index]

	// The series id comes from the stored payload rather than from TMDB: the
	// season list does not carry it, and re-fetching the series just to learn
	// its own id would be a round trip for something already known.
	b.startRequest(ctx, chatID, user, domain.SeasonRef(choice.SeriesID, season.Number), messageID)
}

// startRequest resolves the media and asks the workflow to fetch it.
func (b *Bot) startRequest(ctx context.Context, chatID int64, user *storage.User,
	ref domain.MediaRef, messageID int) {
	spec, err := b.specFor(ctx, ref)
	if err != nil {
		b.log.Error("could not build a spec", "ref", ref, "error", err)
		b.send(ctx, chatID, "Не получилось получить данные о фильме.")
		return
	}

	res, err := b.req.StartRequest(ctx, request.StartInput{Spec: spec, UserID: user.ID})
	if err != nil {
		b.log.Error("could not start a request", "media", spec.String(), "error", err)
		b.send(ctx, chatID, "Не получилось поставить запрос: "+err.Error())
		return
	}

	b.log.Info("request created from telegram",
		"request", res.Request.ID, "media", spec.String(), "user", user.Username)

	var text string
	if res.Reused {
		text = fmt.Sprintf("«%s» уже ищется — подписал вас на уведомления.", spec.DisplayTitle())
	} else {
		text = fmt.Sprintf("Запрос на «%s» создан.\n\nСначала проверю Plex. "+
			"Если этого там нет — буду искать релиз с русским звуком.", spec.DisplayTitle())
	}

	if messageID != 0 {
		b.editText(ctx, chatID, messageID, text)
		return
	}
	b.send(ctx, chatID, text)
}

// specFor builds the frozen spec the workflow carries.
func (b *Bot) specFor(ctx context.Context, ref domain.MediaRef) (domain.MediaSpec, error) {
	if err := ref.ValidForRequest(); err != nil {
		return domain.MediaSpec{}, err
	}

	if ref.Type == domain.MediaTypeMovie {
		title, err := b.tmdb.Movie(ctx, ref.TMDBID)
		if err != nil {
			return domain.MediaSpec{}, err
		}
		return title.ToSpec(), nil
	}

	show, err := b.tmdb.TV(ctx, ref.TMDBID)
	if err != nil {
		return domain.MediaSpec{}, err
	}

	spec := show.ToSpec()
	spec.Ref = ref

	// The season's première, not the series'. A request carrying the series'
	// date would look like media released years ago, and the workflow would
	// search for a season that has not aired.
	for _, season := range show.Seasons {
		if season.Number == ref.SeasonNumber() && season.AirDate != nil {
			spec.ReleasedAt = season.AirDate
		}
	}
	return spec, nil
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

// deliver sends notifications that have not reached a chat yet.
//
// The notifications table is the outbox and this is the only consumer, which
// is what makes the bot restartable: a message written while it was down is
// still there when it comes back, and one that fails to send is retried by the
// next pass because its delivered timestamp is never set.
func (b *Bot) deliver(ctx context.Context) {
	ticker := time.NewTicker(deliveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.deliverOnce(ctx); err != nil {
				b.log.Error("delivery pass failed", "error", err)
			}
		}
	}
}

const (
	deliveryInterval = 15 * time.Second
	deliveryBatch    = 50
)

func (b *Bot) deliverOnce(ctx context.Context) error {
	// Housekeeping rides along with delivery rather than having a loop of its
	// own: expired search sessions and old sessions are the same kind of
	// cleanup, and a second timer for a few DELETEs is not worth the wiring.
	if n, err := b.store.DeleteExpiredSearchSessions(ctx); err != nil {
		b.log.Warn("could not clear expired search sessions", "error", err)
	} else if n > 0 {
		b.log.Debug("cleared expired search sessions", "count", n)
	}
	if n, err := b.store.DeleteExpiredSessions(ctx); err != nil {
		b.log.Warn("could not clear expired sessions", "error", err)
	} else if n > 0 {
		b.log.Debug("cleared expired web sessions", "count", n)
	}

	pending, err := b.store.ListPendingTelegram(ctx, deliveryBatch)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	var delivered []int64

	for _, p := range pending {
		chatID, err := strconv.ParseInt(p.TelegramID, 10, 64)
		if err != nil {
			// A malformed address will never work. Marking it delivered is
			// what stops it being retried forever.
			b.log.Warn("a telegram id is not a number",
				"user", p.UserID, "telegram_id", p.TelegramID)
			delivered = append(delivered, p.ID)
			continue
		}

		if _, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
			ChatID: chatID,
			Text:   notificationText(p),
		}); err != nil {
			// Left undelivered on purpose, so the next pass tries again. A
			// user who blocked the bot produces a permanent failure, which is
			// a small price for not losing a message to a transient one.
			b.log.Warn("could not deliver a notification",
				"notification", p.ID, "user", p.UserID, "error", err)
			continue
		}

		delivered = append(delivered, p.ID)
	}

	if len(delivered) == 0 {
		return nil
	}

	// Marked in one statement, and only after the sends: the reverse order
	// would lose every message in the batch if the process died mid-loop.
	if err := b.store.MarkTelegramDelivered(ctx, delivered); err != nil {
		return err
	}

	b.log.Info("delivered notifications", "count", len(delivered))
	return nil
}

// notificationText renders a notification for a chat.
func notificationText(p storage.PendingTelegram) string {
	var b strings.Builder

	switch p.Kind {
	case storage.NotificationCompleted:
		b.WriteString("✅ ")
	case storage.NotificationFailed:
		b.WriteString("⚠️ ")
	case storage.NotificationSkipped:
		b.WriteString("📚 ")
	case storage.NotificationCanceled:
		b.WriteString("🚫 ")
	default:
		b.WriteString("🔎 ")
	}

	b.WriteString(p.Title)
	if p.Body != "" {
		b.WriteString("\n\n")
		b.WriteString(p.Body)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// user resolves the account behind a chat.
func (b *Bot) user(ctx context.Context, update *models.Update) (*storage.User, error) {
	from := senderOf(update)
	if from == nil {
		return nil, fmt.Errorf("no sender")
	}
	return b.store.UserByTelegram(ctx, telegramIDString(from.ID))
}

// explainSessionError says something useful about an unusable button.
//
// The window between "sent you results" and "you pressed one" is an hour, and
// both failures are ordinary: the session expired, or the button belongs to a
// conversation in another account.
func (b *Bot) explainSessionError(ctx context.Context, chatID int64, err error) {
	switch {
	case context.Cause(ctx) != nil:
		return
	case isExpired(err):
		b.send(ctx, chatID, "Кнопка устарела — результаты поиска живут час. Пришлите название заново.")
	default:
		b.send(ctx, chatID, "Эта кнопка не от вашего поиска. Пришлите название заново.")
	}
}

func senderOf(update *models.Update) *models.User {
	if update.Message != nil {
		return update.Message.From
	}
	if update.CallbackQuery != nil {
		return &update.CallbackQuery.From
	}
	return nil
}

func displayName(u models.User) string {
	name := strings.TrimSpace(u.Username)
	if name != "" {
		return name
	}
	return strings.TrimSpace(u.FirstName)
}

// telegramHTTPClient builds the *http.Client the tgbot library uses to talk
// to api.telegram.org. An empty proxy returns the library's default; an
// http:// or https:// URL is an HTTP CONNECT proxy; a socks5:// URL goes
// through a SOCKS5 dialer. User info on the URL is honored for auth.
//
// The timeout matches the library's poll interval (one minute), so a
// hung connection fails within a poll rather than across many.
func telegramHTTPClient(proxyURL string) (*http.Client, error) {
	timeout := time.Minute
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return &http.Client{Timeout: timeout}, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse telegram.proxy: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		return &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy: http.ProxyURL(u),
			},
		}, nil
	case "socks5":
		var auth *xproxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &xproxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := xproxy.SOCKS5("tcp", u.Host, auth, xproxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("build the SOCKS5 dialer: %w", err)
		}
		return &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{Dial: dialer.Dial},
		}, nil
	default:
		return nil, fmt.Errorf("telegram.proxy: unsupported scheme %q (want http, https, or socks5)", u.Scheme)
	}
}
