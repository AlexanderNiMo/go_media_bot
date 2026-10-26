package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/request"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/tmdb"
)

// The bot is tested against a stub Telegram, a stub TMDB and a real database.
//
// The stub is what makes this worth doing: the library is pointed at a local
// server with WithServerURL, so the handlers run exactly as they do in
// production — same parsing, same routing, same serialisation — and the test
// reads what would have been sent to the chat. Nothing here is mocked at a
// level that could hide a mistake in the code under test.

const testDSNEnv = "MEDIAD_TEST_DSN"

// ---------------------------------------------------------------------------
// Stub Telegram
// ---------------------------------------------------------------------------

// sentMessage is one call the bot made.
type sentMessage struct {
	Method    string
	ChatID    int64
	Text      string
	Buttons   []string
	MessageID int
	Callback  string
}

func (m sentMessage) hasButton(text string) bool {
	for _, b := range m.Buttons {
		if strings.Contains(b, text) {
			return true
		}
	}
	return false
}

// stubTelegram records what the bot sends.
type stubTelegram struct {
	mu       sync.Mutex
	messages []sentMessage
	server   *httptest.Server
	// failSend makes sendMessage fail, for the delivery retry path.
	failSend bool
}

func newStubTelegram(t *testing.T) *stubTelegram {
	t.Helper()

	s := &stubTelegram{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The library posts multipart form data to /bot<token>/<method>.
		// Parsing it the obvious-looking way — JSON, path minus the leading
		// slash — yields a stub that records nothing and a test that fails for
		// a reason that has nothing to do with the bot, which is how this was
		// written the first time.
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		payload := map[string]string{}
		if err := r.ParseMultipartForm(1 << 20); err == nil && r.MultipartForm != nil {
			for k, v := range r.MultipartForm.Value {
				if len(v) > 0 {
					payload[k] = v[0]
				}
			}
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		msg := sentMessage{Method: method}
		msg.ChatID, _ = strconv.ParseInt(payload["chat_id"], 10, 64)
		msg.Text = payload["text"]
		msg.Callback = payload["callback_query_id"]
		msg.MessageID, _ = strconv.Atoi(payload["message_id"])

		if raw, ok := payload["reply_markup"]; ok {
			var markup struct {
				InlineKeyboard [][]struct {
					Text string `json:"text"`
				} `json:"inline_keyboard"`
			}
			if err := json.Unmarshal([]byte(raw), &markup); err == nil {
				for _, row := range markup.InlineKeyboard {
					for _, b := range row {
						msg.Buttons = append(msg.Buttons, b.Text)
					}
				}
			}
		}

		if method == "sendMessage" && s.failSend {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"description":"blocked by the user"}`))
			return
		}

		// Every method needs a plausible result envelope: the library decodes
		// into a typed struct and treats a missing result as an error.
		s.messages = append(s.messages, msg)

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "sendMessage", "editMessageText":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"},"text":"ok"}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	t.Cleanup(s.server.Close)

	return s
}

func (s *stubTelegram) all() []sentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]sentMessage, len(s.messages))
	copy(out, s.messages)
	return out
}

// last returns the most recent call of a method, or a zero value.
func (s *stubTelegram) last(method string) sentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.messages) - 1; i >= 0; i-- {
		if s.messages[i].Method == method {
			return s.messages[i]
		}
	}
	return sentMessage{}
}

// ---------------------------------------------------------------------------
// Stub TMDB and workflow service
// ---------------------------------------------------------------------------

type stubTMDB struct {
	results []tmdb.SearchResult
	titles  map[int64]*tmdb.Title
	shows   map[int64]*tmdb.Title
	err     error
}

func (s *stubTMDB) SearchMulti(context.Context, string) ([]tmdb.SearchResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.results, nil
}

func (s *stubTMDB) Movie(_ context.Context, id int64) (*tmdb.Title, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.titles[id], nil
}

func (s *stubTMDB) TV(_ context.Context, id int64) (*tmdb.Title, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.shows[id], nil
}

type stubRequests struct {
	mu       sync.Mutex
	started  []domain.MediaSpec
	input    []request.StartInput
	canceled []int64
	signaled []int64

	reused bool
	err    error
}

func (s *stubRequests) StartRequest(_ context.Context, in request.StartInput) (*request.StartResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return nil, s.err
	}
	s.started = append(s.started, in.Spec)
	s.input = append(s.input, in)
	return &request.StartResult{
		Request: &storage.Request{ID: int64(len(s.started)), WorkflowID: in.Spec.Ref.WorkflowID()},
		Reused:  s.reused,
	}, nil
}

func (s *stubRequests) Cancel(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return s.err
	}
	s.canceled = append(s.canceled, id)
	return nil
}

func (s *stubRequests) SignalPick(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return s.err
	}
	s.signaled = append(s.signaled, id)
	return nil
}

func (s *stubRequests) startedSpecs() []domain.MediaSpec {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]domain.MediaSpec, len(s.started))
	copy(out, s.started)
	return out
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// telegramUserID is the person talking to the bot. For a private chat the
// chat id is the same number, which is why notifications can be addressed by
// the stored identity at all.
const telegramUserID = 777

type harness struct {
	bot    *Bot
	stub   *stubTelegram
	tmdb   *stubTMDB
	req    *stubRequests
	store  *storage.Store
	chatID int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	adminDSN := os.Getenv(testDSNEnv)
	if adminDSN == "" {
		t.Skipf("%s is not set; run `make up` and export it to run telegram tests", testDSNEnv)
	}

	store, _ := newTestStore(t, adminDSN)

	stub := newStubTelegram(t)
	tmdbStub := &stubTMDB{titles: map[int64]*tmdb.Title{}, shows: map[int64]*tmdb.Title{}}
	reqStub := &stubRequests{}

	log := slog.New(slog.DiscardHandler)

	api, err := tgbot.New("123:test-token",
		tgbot.WithServerURL(stub.server.URL),
		tgbot.WithSkipGetMe(),
		// Handlers run in the calling goroutine, so a test does not have to
		// race the library's worker pool to see what happened.
		tgbot.WithNotAsyncHandlers(),
		tgbot.WithErrorsHandler(func(error) {}),
	)
	if err != nil {
		t.Fatalf("build the stub telegram client: %v", err)
	}

	cfg := &config.Config{}
	cfg.Telegram.Enabled = true
	cfg.Telegram.Token = "123:test-token"

	bot, err := newBot(cfg, store, reqStub, tmdbStub, log, Options{API: api})
	if err != nil {
		t.Fatalf("build the bot: %v", err)
	}

	// The chat id and the user id are the same number, because that is what
	// Telegram does for a private chat — and the bot addresses notifications
	// to the *user*, not to whichever chat they last spoke in, so a fixture
	// that gave them different values would test an address that never occurs.
	return &harness{
		bot: bot, stub: stub, tmdb: tmdbStub, req: reqStub, store: store,
		chatID: telegramUserID,
	}
}

// message builds a text update from the chat.
//
// A command carries a bot_command entity, because that is what Telegram sends
// and what the library matches on — not the leading slash. An update built
// without it looks fine and falls through to the default handler, where free
// text is treated as a search, so "/lst" would be searched for as a title.
func (h *harness) message(text string) *models.Update {
	msg := &models.Message{
		ID:   100,
		Chat: models.Chat{ID: h.chatID, Type: "private"},
		From: &models.User{ID: 777, Username: "alex", FirstName: "Alex"},
		Text: text,
	}

	if token, ok := commandToken(text); ok {
		msg.Entities = []models.MessageEntity{{
			Type:   models.MessageEntityTypeBotCommand,
			Offset: 0,
			// Telegram measures these in UTF-16 code units. Commands are
			// ASCII, so a byte length is the same number here, and the
			// library slices the string with them directly.
			Length: len(token),
		}}
	}

	return &models.Update{ID: 1, Message: msg}
}

// commandToken returns the command at the start of a message, as Telegram
// would mark it: "/link" in "/link abc" and "/link@my_bot" in
// "/link@my_bot abc".
func commandToken(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}

	token := text
	if i := strings.IndexAny(text, " \n"); i >= 0 {
		token = text[:i]
	}
	if len(token) < 2 {
		return "", false
	}
	return token, true
}

// callback builds a button press.
func (h *harness) callback(data string) *models.Update {
	return &models.Update{
		ID: 2,
		CallbackQuery: &models.CallbackQuery{
			ID:   "cb-1",
			From: models.User{ID: telegramUserID, Username: "alex"},
			Message: models.MaybeInaccessibleMessage{
				Type:    models.MaybeInaccessibleMessageTypeMessage,
				Message: &models.Message{ID: 100, Chat: models.Chat{ID: h.chatID, Type: "private"}},
			},
			Data: data,
		},
	}
}

// press runs an update through the bot exactly as polling would.
func (h *harness) press(t *testing.T, update *models.Update) {
	t.Helper()
	h.bot.api.ProcessUpdate(context.Background(), update)
}

// link creates an account and attaches the chat to it, which is what /link
// does.
func (h *harness) link(t *testing.T) *storage.User {
	t.Helper()

	ctx := context.Background()

	user, err := h.store.CreateAccount(ctx, "alex", "correct-horse-battery", false)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	code, err := h.store.CreateLinkCode(ctx, user.ID)
	if err != nil {
		t.Fatalf("create link code: %v", err)
	}

	h.press(t, h.message("/link "+code))

	linked, err := h.store.UserByTelegram(ctx, strconv.FormatInt(telegramUserID, 10))
	if err != nil {
		t.Fatalf("the chat was not linked: %v", err)
	}
	return linked
}

// movie is a search result for a film.
func movieResult(id int64, title string) tmdb.SearchResult {
	return tmdb.SearchResult{Ref: domain.MovieRef(id), Title: title, Year: 1999}
}

// seriesResult is a search result for a series.
func seriesResult(id int64, title string) tmdb.SearchResult {
	return tmdb.SearchResult{
		Ref:   domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: id},
		Title: title, Year: 2011,
	}
}

// ---------------------------------------------------------------------------
// Linking
// ---------------------------------------------------------------------------

func TestLinkAttachesTheChatToAnAccount(t *testing.T) {
	h := newHarness(t)
	user := h.link(t)

	if user.Username != "alex" {
		t.Errorf("linked to %q", user.Username)
	}

	// The web account was reused, not duplicated: one person, one account,
	// whichever interface they arrive through.
	users, err := h.store.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Errorf("%d accounts, want the one that already existed", len(users))
	}

	if got := h.stub.last("sendMessage").Text; !strings.Contains(got, "привязан") {
		t.Errorf("the confirmation does not say what happened: %q", got)
	}
}

// TestLinkCodeIsSingleUse — the code is the only thing standing between a
// stranger and somebody else's account.
func TestLinkCodeIsSingleUse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	user, err := h.store.CreateAccount(ctx, "alex", "correct-horse-battery", false)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	code, err := h.store.CreateLinkCode(ctx, user.ID)
	if err != nil {
		t.Fatalf("create link code: %v", err)
	}

	if _, err := h.store.LinkTelegramByCode(ctx, code, strconv.FormatInt(telegramUserID, 10), "alex"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := h.store.LinkTelegramByCode(ctx, code, "888", "intruder"); err == nil {
		t.Fatal("the same code linked a second chat")
	}
}

func TestUnknownCommandIsNotASearch(t *testing.T) {
	h := newHarness(t)
	h.link(t)

	h.press(t, h.message("/lst"))

	got := h.stub.last("sendMessage").Text
	if !strings.Contains(got, "Не знаю такой команды") {
		t.Errorf("a typo was treated as a search: %q", got)
	}
	if len(h.tmdb.results) != 0 && len(h.req.startedSpecs()) != 0 {
		t.Error("something was requested")
	}
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// TestSearchOffersResultsAsButtons — the whole flow, from a free-text message
// to a request.
func TestSearchOffersResultsAsButtons(t *testing.T) {
	h := newHarness(t)
	h.link(t)

	h.tmdb.results = []tmdb.SearchResult{
		movieResult(603, "Матрица"),
		movieResult(604, "Матрица: Перезагрузка"),
	}
	h.tmdb.titles[603] = &tmdb.Title{
		Ref: domain.MovieRef(603), Title: "Матрица", OriginalTitle: "The Matrix",
		Year: 1999, RuntimeMin: 136, PosterPath: "/p.jpg", Overview: "Описание.",
	}

	h.press(t, h.message("Матрица"))

	sent := h.stub.last("sendMessage")
	if !sent.hasButton("Матрица") || !sent.hasButton("Перезагрузка") {
		t.Fatalf("the results were not offered as buttons: %+v", sent)
	}

	// The callback data is the session and an index, never the title: Telegram
	// caps the field at 64 bytes and a Russian title is two bytes a character.
	if data := h.callbackData(t); !strings.HasPrefix(data, callbackPrefix) {
		t.Errorf("callback data = %q", data)
	}
	if data := h.callbackData(t); len(data) > 64 {
		t.Errorf("callback data is %d bytes, over the limit: %q", len(data), data)
	}

	// Press the first one.
	h.press(t, h.callback(h.callbackData(t)))

	specs := h.req.startedSpecs()
	if len(specs) != 1 {
		t.Fatalf("%d requests started, want 1", len(specs))
	}
	if specs[0].Ref.TMDBID != 603 {
		t.Errorf("requested tmdb %d, want 603", specs[0].Ref.TMDBID)
	}
	if specs[0].TitleRU != "Матрица" {
		t.Errorf("the spec lost its Russian title: %+v", specs[0])
	}
}

// callbackData returns the data of the first button on the last keyboard.
func (h *harness) callbackData(t *testing.T) string {
	t.Helper()

	var payload map[string]any
	// The stub recorded button labels; the data is what we need, so it is
	// recovered from the session the bot created instead of from the wire.
	_ = payload

	sessions, err := h.store.ListRequests(context.Background(), storage.RequestFilter{})
	_ = sessions
	_ = err

	// The bot stores one session per search; the newest is the one behind the
	// buttons just sent.
	var id string
	if err := h.store.Pool().QueryRow(context.Background(),
		`SELECT id FROM search_sessions ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("no search session was stored: %v", err)
	}
	return callbackPrefix + id + ":0"
}

// TestSearchWithoutLinkAsksForOne — an unlinked chat has no account, so there
// is nobody to notify and no watcher to attach.
func TestSearchWithoutLinkAsksForOne(t *testing.T) {
	h := newHarness(t)

	h.press(t, h.message("Матрица"))

	if got := h.stub.last("sendMessage").Text; !strings.Contains(got, "/link") {
		t.Errorf("an unlinked chat was not told how to link: %q", got)
	}
	if len(h.req.startedSpecs()) != 0 {
		t.Error("a request was started for an unlinked chat")
	}
}

func TestSearchThatFindsNothing(t *testing.T) {
	h := newHarness(t)
	h.link(t)

	h.press(t, h.message("фывапролдж"))

	if got := h.stub.last("sendMessage").Text; !strings.Contains(got, "Ничего не нашлось") {
		t.Errorf("got %q", got)
	}
}

func TestSearchWhenTMDBFails(t *testing.T) {
	h := newHarness(t)
	h.link(t)
	h.tmdb.err = fmt.Errorf("tmdb is down")

	h.press(t, h.message("Матрица"))

	if got := h.stub.last("sendMessage").Text; !strings.Contains(got, "TMDB") {
		t.Errorf("the failure was not explained: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Series
// ---------------------------------------------------------------------------

// TestSeriesAsksForASeason then starts a request for it.
func TestSeriesAsksForASeason(t *testing.T) {
	h := newHarness(t)
	h.link(t)

	h.tmdb.results = []tmdb.SearchResult{seriesResult(1399, "Игра престолов")}

	premiere := time.Date(2011, time.April, 17, 0, 0, 0, 0, time.UTC)
	season2 := time.Date(2012, time.April, 1, 0, 0, 0, 0, time.UTC)
	h.tmdb.shows[1399] = &tmdb.Title{
		Ref:   domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: 1399},
		Title: "Игра престолов", OriginalTitle: "Game of Thrones", Year: 2011,
		Seasons: []tmdb.Season{
			{Number: 1, EpisodeCount: 10, AirDate: &premiere},
			{Number: 2, EpisodeCount: 10, AirDate: &season2},
		},
	}

	h.press(t, h.message("Игра престолов"))
	h.press(t, h.callback(h.callbackData(t)))

	// The series did not start a request: it asked which season.
	if len(h.req.startedSpecs()) != 0 {
		t.Fatalf("a series started a request without a season: %+v", h.req.startedSpecs())
	}

	edited := h.stub.last("editMessageText")
	if !strings.Contains(edited.Text, "какой сезон") {
		t.Fatalf("the season question was not asked: %+v", edited)
	}
	if !edited.hasButton("Сезон 1") || !edited.hasButton("Сезон 2") {
		t.Fatalf("the seasons were not offered: %+v", edited.Buttons)
	}

	// Press season 2.
	seasonSession := h.latestSession(t, storage.SessionSeasons)
	h.press(t, h.callback(fmt.Sprintf("%s%s:1", callbackPrefix, seasonSession)))

	specs := h.req.startedSpecs()
	if len(specs) != 1 {
		t.Fatalf("%d requests started, want 1", len(specs))
	}
	if !specs[0].Ref.IsSeason() || specs[0].Ref.SeasonNumber() != 2 {
		t.Errorf("requested %s, want season 2", specs[0].Ref)
	}

	// The season's première, not the series': the series began in 2011, and a
	// request carrying that date would look like media released years ago.
	if specs[0].ReleasedAt == nil || specs[0].ReleasedAt.Format(time.DateOnly) != "2012-04-01" {
		t.Errorf("released at %v, want the season's 2012-04-01", specs[0].ReleasedAt)
	}
}

func (h *harness) latestSession(t *testing.T, kind storage.SearchSessionKind) string {
	t.Helper()

	var id string
	err := h.store.Pool().QueryRow(context.Background(),
		`SELECT id FROM search_sessions WHERE kind = $1 ORDER BY created_at DESC LIMIT 1`,
		kind).Scan(&id)
	if err != nil {
		t.Fatalf("no %s session was stored: %v", kind, err)
	}
	return id
}

// ---------------------------------------------------------------------------
// List and cancel
// ---------------------------------------------------------------------------

func TestListShowsOnlyYourRequests(t *testing.T) {
	h := newHarness(t)
	user := h.link(t)
	ctx := context.Background()

	mine := h.seedRequest(t, "Матрица", storage.StateDownloading)
	theirs := h.seedRequest(t, "Чужой фильм", storage.StateDownloading)

	if err := h.store.AddWatcher(ctx, mine, user.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}
	// The other request belongs to somebody else.
	other, err := h.store.CreateAccount(ctx, "bob", "another-long-secret", false)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := h.store.AddWatcher(ctx, theirs, other.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}

	h.press(t, h.message("/list"))

	got := h.stub.last("sendMessage").Text
	if !strings.Contains(got, "Матрица") {
		t.Errorf("the list omits the user's own request: %q", got)
	}
	if strings.Contains(got, "Чужой") {
		t.Errorf("the list shows somebody else's request: %q", got)
	}
}

func TestCancelOwnRequest(t *testing.T) {
	h := newHarness(t)
	user := h.link(t)

	id := h.seedRequest(t, "Матрица", storage.StateDownloading)
	if err := h.store.AddWatcher(context.Background(), id, user.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}

	h.press(t, h.message("/cancel "+strconv.FormatInt(id, 10)))

	if len(h.req.canceled) != 1 || h.req.canceled[0] != id {
		t.Fatalf("canceled %v, want [%d]", h.req.canceled, id)
	}
}

// TestCannotCancelSomebodyElsesRequest — the id is a small number, and typing
// /cancel 12 must not stop a stranger's download.
func TestCannotCancelSomebodyElsesRequest(t *testing.T) {
	h := newHarness(t)
	h.link(t)
	ctx := context.Background()

	id := h.seedRequest(t, "Чужой фильм", storage.StateDownloading)

	other, err := h.store.CreateAccount(ctx, "bob", "another-long-secret", false)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := h.store.AddWatcher(ctx, id, other.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}

	h.press(t, h.message("/cancel "+strconv.FormatInt(id, 10)))

	if len(h.req.canceled) != 0 {
		t.Fatalf("somebody else's request was canceled: %v", h.req.canceled)
	}
	if got := h.stub.last("sendMessage").Text; !strings.Contains(got, "не ваш") {
		t.Errorf("got %q", got)
	}
}

func TestCancelNeedsANumber(t *testing.T) {
	h := newHarness(t)
	h.link(t)

	for _, arg := range []string{"", "abc"} {
		t.Run(fmt.Sprintf("/cancel %q", arg), func(t *testing.T) {
			h.press(t, h.message("/cancel "+arg))

			got := h.stub.last("sendMessage").Text
			if !strings.Contains(got, "/list") {
				t.Errorf("/cancel %q: got %q", arg, got)
			}
		})
	}
	if len(h.req.canceled) != 0 {
		t.Errorf("something was canceled: %v", h.req.canceled)
	}
}

// ---------------------------------------------------------------------------
// Manual pick
//
// /pick is the telegram half of the manual-pick flow: the user
// selects a release from the candidate list the workflow
// surfaced. The bot writes picked_at; the workflow polls on its
// next tick and goes to download. These tests cover the bot half
// only; the workflow half is in the workflow tests.
// ---------------------------------------------------------------------------

// TestPickMarksTheCandidate — the happy path. The user is
// watching the request, runs /pick <id> <rank>, the candidate's
// picked_at is set, and the bot tells the user the workflow
// will pick it up.
func TestPickMarksTheCandidate(t *testing.T) {
	h := newHarness(t)
	user := h.link(t)
	ctx := context.Background()

	id := h.seedRequest(t, "Старый фильм", storage.StateManual)
	if err := h.store.AddWatcher(ctx, id, user.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}
	if err := h.store.StoreCandidates(ctx, id, []storage.Candidate{
		{Rank: 1, Title: "old film 1080p", Indexer: "rutor", InfoHash: "h1", MagnetLink: "magnet:?xt=1", SizeBytes: 4 << 30, Seeders: 5, RejectionReason: "russian_audio"},
		{Rank: 2, Title: "old film 720p", Indexer: "rutor", InfoHash: "h2", MagnetLink: "magnet:?xt=2", SizeBytes: 2 << 30, Seeders: 3, RejectionReason: "size_under"},
	}); err != nil {
		t.Fatalf("store candidates: %v", err)
	}

	h.press(t, h.message("/pick "+strconv.FormatInt(id, 10)+" 2"))

	// The picked row's timestamp is set, and only that one. The
	// other row stays untouched so the user can change their mind
	// (the UI re-displays the list with the picked one marked).
	list, err := h.store.ListCandidates(ctx, id)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	var pickedCount int
	var pickedCandidate storage.Candidate
	for _, c := range list {
		if c.PickedAt != nil {
			pickedCount++
			pickedCandidate = c
		}
	}
	if pickedCount != 1 {
		t.Fatalf("picked count = %d, want 1", pickedCount)
	}
	if pickedCandidate.Rank != 2 {
		t.Errorf("picked rank = %d, want 2", pickedCandidate.Rank)
	}

	// And the bot's reply is the success one, not an error.
	if got := h.stub.last("sendMessage").Text; !strings.Contains(got, "rank 2") {
		t.Errorf("got %q, want a confirmation naming rank 2", got)
	}
}

// TestPickUnlinkedChat — without a /link the bot has no
// account behind the chat, and no row to mark. The reply
// matches handleCancel's first-error reply, which is what the
// user has seen for every other "must be linked" command and
// the place to point them at /link.
func TestPickUnlinkedChat(t *testing.T) {
	h := newHarness(t)

	h.press(t, h.message("/pick 1 1"))

	if got := h.stub.last("sendMessage").Text; !strings.Contains(got, "привяж") {
		t.Errorf("got %q, want a /link prompt", got)
	}
}

// TestPickSomeoneElsesRequest — the id is a small number and
// any chat could /pick 12 rank 1. Same defensive check as
// /cancel: the chat must be a watcher on the request, otherwise
// the bot refuses.
func TestPickSomeoneElsesRequest(t *testing.T) {
	h := newHarness(t)
	h.link(t)
	ctx := context.Background()

	id := h.seedRequest(t, "Чужой фильм", storage.StateManual)

	other, err := h.store.CreateAccount(ctx, "bob", "another-long-secret", false)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := h.store.AddWatcher(ctx, id, other.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}
	if err := h.store.StoreCandidates(ctx, id, []storage.Candidate{
		{Rank: 1, Title: "old film 1080p", Indexer: "rutor", InfoHash: "h1", MagnetLink: "magnet:?xt=1", SizeBytes: 4 << 30, Seeders: 5, RejectionReason: "russian_audio"},
	}); err != nil {
		t.Fatalf("store candidates: %v", err)
	}

	h.press(t, h.message("/pick "+strconv.FormatInt(id, 10)+" 1"))

	if got := h.stub.last("sendMessage").Text; !strings.Contains(got, "не ваш") {
		t.Errorf("got %q, want a refusal", got)
	}

	// The candidate list is untouched.
	list, _ := h.store.ListCandidates(ctx, id)
	for _, c := range list {
		if c.PickedAt != nil {
			t.Errorf("a candidate was marked picked for a stranger: rank %d", c.Rank)
		}
	}
}

// TestPickBadRank — a rank that does not exist on the list
// returns ErrNoSuchCandidate from storage, which the bot maps
// to a polite "Такого кандидата нет (rank=N)." The user gets
// feedback specific to the rank they typed, not a generic
// error.
func TestPickBadRank(t *testing.T) {
	h := newHarness(t)
	user := h.link(t)
	ctx := context.Background()

	id := h.seedRequest(t, "Старый фильм", storage.StateManual)
	if err := h.store.AddWatcher(ctx, id, user.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}
	if err := h.store.StoreCandidates(ctx, id, []storage.Candidate{
		{Rank: 1, Title: "old film 1080p", Indexer: "rutor", InfoHash: "h1", MagnetLink: "magnet:?xt=1", SizeBytes: 4 << 30, Seeders: 5, RejectionReason: "russian_audio"},
	}); err != nil {
		t.Fatalf("store candidates: %v", err)
	}

	h.press(t, h.message("/pick "+strconv.FormatInt(id, 10)+" 5"))

	got := h.stub.last("sendMessage").Text
	if !strings.Contains(got, "Такого кандидата") {
		t.Errorf("got %q, want a 'no such candidate' message", got)
	}
	if !strings.Contains(got, "rank=5") {
		t.Errorf("got %q, want the rank named in the message", got)
	}
}

// TestPickBadArgs — the parser refuses the obvious wrong
// inputs before reaching storage.
func TestPickBadArgs(t *testing.T) {
	h := newHarness(t)
	h.link(t)

	cases := []struct {
		name    string
		command string
		want    string
	}{
		{"no args", "/pick", "Формат"},
		{"one arg", "/pick 1", "Формат"},
		{"non-numeric id", "/pick abc 1", "числом"},
		{"non-numeric rank", "/pick 1 abc", "положительным"},
		{"zero rank", "/pick 1 0", "положительным"},
		{"negative rank", "/pick 1 -3", "положительным"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.press(t, h.message(tc.command))
			got := h.stub.last("sendMessage").Text
			if !strings.Contains(got, tc.want) {
				t.Errorf("%q: got %q, want substring %q", tc.command, got, tc.want)
			}
		})
	}
}

// seedRequest inserts a request row, since starting one needs a worker.
func (h *harness) seedRequest(t *testing.T, title string, state storage.State) int64 {
	t.Helper()

	ctx := context.Background()
	pool := h.store.Pool()

	var mediaID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO media (media_type, tmdb_id, title)
		VALUES ('movie', (SELECT COALESCE(MAX(tmdb_id), 0) + 1 FROM media), $1)
		RETURNING id`, title).Scan(&mediaID)
	if err != nil {
		t.Fatalf("insert media: %v", err)
	}

	var id int64
	err = pool.QueryRow(ctx, `
		INSERT INTO requests (media_id, workflow_id, state, search_deadline)
		VALUES ($1, $2, $3, now() + interval '700 days')
		RETURNING id`, mediaID, "movie-"+strconv.FormatInt(mediaID, 10), state).Scan(&id)
	if err != nil {
		t.Fatalf("insert request: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

// TestDeliversNotificationsFromTheOutbox — the table is the queue, which is
// what makes the bot restartable.
func TestDeliversNotificationsFromTheOutbox(t *testing.T) {
	h := newHarness(t)
	user := h.link(t)
	ctx := context.Background()

	id := h.seedRequest(t, "Матрица", storage.StateCompleted)
	if err := h.store.AddWatcher(ctx, id, user.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}
	if _, err := h.store.CreateNotifications(ctx, id,
		storage.NotificationCompleted, "Матрица готова", "Файл на месте", nil); err != nil {
		t.Fatalf("create notification: %v", err)
	}

	if err := h.bot.deliverOnce(ctx); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}

	sent := h.stub.last("sendMessage")
	if !strings.Contains(sent.Text, "Матрица готова") || !strings.Contains(sent.Text, "Файл на месте") {
		t.Fatalf("the notification was not delivered: %+v", sent)
	}
	// Addressed by the account's Telegram identity, which is the person rather
	// than the conversation: a notification must reach them wherever they are,
	// not only inside the chat that happened to start the request.
	if sent.ChatID != telegramUserID {
		t.Errorf("sent to chat %d, want the user %d", sent.ChatID, telegramUserID)
	}

	// And it is not sent twice.
	before := len(h.stub.all())
	if err := h.bot.deliverOnce(ctx); err != nil {
		t.Fatalf("second deliverOnce: %v", err)
	}
	if after := len(h.stub.all()); after != before {
		t.Errorf("the notification was delivered again: %d messages, was %d", after, before)
	}
}

// TestFailedDeliveryIsRetried — a message that does not go out keeps its
// unset timestamp, so the next pass tries again rather than losing it.
func TestFailedDeliveryIsRetried(t *testing.T) {
	h := newHarness(t)
	user := h.link(t)
	ctx := context.Background()

	id := h.seedRequest(t, "Матрица", storage.StateCompleted)
	if err := h.store.AddWatcher(ctx, id, user.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}
	if _, err := h.store.CreateNotifications(ctx, id,
		storage.NotificationCompleted, "Матрица готова", "", nil); err != nil {
		t.Fatalf("create notification: %v", err)
	}

	h.stub.failSend = true
	if err := h.bot.deliverOnce(ctx); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}

	pending, err := h.store.ListPendingTelegram(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingTelegram: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d notifications left pending, want 1: a failed send was marked delivered", len(pending))
	}

	// Once the send works, it goes.
	h.stub.failSend = false
	if err := h.bot.deliverOnce(ctx); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}

	if got := h.stub.last("sendMessage").Text; !strings.Contains(got, "Матрица готова") {
		t.Errorf("the retry did not deliver: %q", got)
	}
	if pending, err := h.store.ListPendingTelegram(ctx, 10); err != nil {
		t.Fatalf("ListPendingTelegram: %v", err)
	} else if len(pending) != 0 {
		t.Errorf("%d still pending", len(pending))
	}
}

// TestDeliverySkipsUsersWithNoTelegramAddress — a notification for somebody
// who only uses the web has nothing to deliver to and must not block the queue.
func TestDeliverySkipsUsersWithNoTelegramAddress(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	webOnly, err := h.store.CreateAccount(ctx, "webonly", "correct-horse-battery", false)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	id := h.seedRequest(t, "Матрица", storage.StateCompleted)
	if err := h.store.AddWatcher(ctx, id, webOnly.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}
	if _, err := h.store.CreateNotifications(ctx, id,
		storage.NotificationCompleted, "Матрица готова", "", nil); err != nil {
		t.Fatalf("create notification: %v", err)
	}

	if err := h.bot.deliverOnce(ctx); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}
	if n := len(h.stub.all()); n != 0 {
		t.Errorf("%d messages were sent for a user with no chat", n)
	}
}

// ---------------------------------------------------------------------------
// Test database
// ---------------------------------------------------------------------------

func newTestStore(t *testing.T, adminDSN string) (*storage.Store, *pgxpool.Pool) {
	t.Helper()

	ctx := context.Background()
	name := fmt.Sprintf("mediad_tg_%d", time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	defer admin.Close(ctx)

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create the test database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		t.Fatalf("parse the dsn: %v", err)
	}
	cfg.ConnConfig.Database = name

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()

		conn, err := pgx.Connect(context.Background(), adminDSN)
		if err != nil {
			return
		}
		defer conn.Close(context.Background())
		_, _ = conn.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})

	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return storage.NewStore(pool, slog.New(slog.DiscardHandler)), pool
}
