package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/tmdb"
)

// The keyboard and message helpers, and the small pieces of text the bot says
// about a request.

// callbackPrefix marks a button as ours.
//
// Every callback the bot receives goes through one handler, so the data has to
// say what it is. The prefix also lets the library route on it, which is why
// it is registered as a prefix match rather than handled in the default.
const callbackPrefix = "sel:"

// seasonChoice is what a seasons session holds.
//
// The series id travels with the season list because the list itself does not
// contain it — tmdb.Season has a number, a name and an air date, and nothing
// that says which show it belongs to. Recovering it would mean fetching the
// series again to learn something already known when the message was sent.
type seasonChoice struct {
	SeriesID int64
	Title    string
	Seasons  []tmdb.Season
}

// maxButtons caps how many results are offered.
//
// Telegram allows more, but a keyboard of twenty titles is unreadable on a
// phone, and the right fix for "it is not in the first ten" is a better query
// rather than a longer list.
const maxButtons = 10

// resultsKeyboard builds the buttons for a page of search results.
func resultsKeyboard(sessionID string, results []tmdb.SearchResult) *models.InlineKeyboardMarkup {
	rows := make([][]models.InlineKeyboardButton, 0, maxButtons)

	for i, r := range results {
		if i >= maxButtons {
			break
		}

		label := r.Title
		if r.Year > 0 {
			label += fmt.Sprintf(" (%d)", r.Year)
		}
		if r.Ref.Type == domain.MediaTypeTV {
			label += " · сериал"
		}
		if r.VoteAverage > 0 {
			// TMDB's user score (0–10). Not IMDb, but the same number a
			// catalogue usually shows next to a title; star makes it scannable
			// in a list of buttons.
			label += fmt.Sprintf(" · ⭐ %.1f", r.VoteAverage)
		}

		rows = append(rows, []models.InlineKeyboardButton{{
			Text: truncateRunes(label, 60),
			// The index, not the title: it is a few bytes against a limit of
			// sixty-four, and the payload behind it lives in the database.
			CallbackData: fmt.Sprintf("%s%s:%d", callbackPrefix, sessionID, i),
		}})
	}

	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// seasonsKeyboard builds the buttons for a season list.
//
// Two per row: season numbers are short, and a single column of twenty rows
// pushes the message off the screen.
func seasonsKeyboard(sessionID string, seasons []tmdb.Season) *models.InlineKeyboardMarkup {
	var rows [][]models.InlineKeyboardButton
	var current []models.InlineKeyboardButton

	for i, s := range seasons {
		if i >= 30 {
			break
		}

		label := fmt.Sprintf("Сезон %d", s.Number)
		if s.EpisodeCount > 0 {
			label += fmt.Sprintf(" (%d)", s.EpisodeCount)
		}

		current = append(current, models.InlineKeyboardButton{
			Text:         label,
			CallbackData: fmt.Sprintf("%s%s:%d", callbackPrefix, sessionID, i),
		})

		if len(current) == 2 {
			rows = append(rows, current)
			current = nil
		}
	}
	if len(current) > 0 {
		rows = append(rows, current)
	}

	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// candidatePrefix marks a candidate button as ours.
//
// A search button carries a session id and an index into a stored
// payload. A candidate button needs no payload at all: the request id
// and the rank are both things the database already knows, so the data
// stays a couple of dozen bytes against Telegram's limit of sixty-four.
const candidatePrefix = "cnd:"

// candidatesKeyboard builds the buttons for a request's candidate list.
//
// One button per release, in rank order: the rank is what the user
// picks and the title is what they recognise. The seeders and the size
// come along, because two releases with the same title are two
// different downloads.
//
// Why the auto-selector refused each release is not on the button — it
// is in the message above the keyboard, one line per candidate, which
// is where the detail belongs. A button is for choosing, not for
// reading.
func candidatesKeyboard(requestID int64, candidates []storage.Candidate) *models.InlineKeyboardMarkup {
	rows := make([][]models.InlineKeyboardButton, 0, len(candidates))

	for _, c := range candidates {
		label := fmt.Sprintf("%d. %s", c.Rank, c.Title)
		if c.Seeders > 0 {
			label += fmt.Sprintf(" · %d сидеров", c.Seeders)
		}
		if c.SizeBytes > 0 {
			label += " · " + humanBytes(c.SizeBytes)
		}

		rows = append(rows, []models.InlineKeyboardButton{{
			Text: truncateRunes(label, 60),
			// The request id and the rank, not the title: the title is in
			// the label, where a human reads it, and it would blow past
			// the byte limit here.
			CallbackData: fmt.Sprintf("%s%d:%d", candidatePrefix, requestID, c.Rank),
		}})
	}

	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// emptyKeyboard is the markup that takes the buttons off a message.
//
// Telegram removes an inline keyboard when it is handed an empty one.
// Passing null is not what the API documents for editing a message, so
// it is not what this does.
func emptyKeyboard() *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

// reply answers the message an update came from.
func (b *Bot) reply(ctx context.Context, update *models.Update, text string) {
	if msg := messageOf(update); msg != nil {
		b.send(ctx, msg.Chat.ID, text)
	}
}

// replyMarkup answers with buttons.
func (b *Bot) replyMarkup(ctx context.Context, update *models.Update, text string,
	markup *models.InlineKeyboardMarkup) {
	if msg := messageOf(update); msg != nil {
		b.sendMarkup(ctx, msg.Chat.ID, text, markup)
	}
}

func (b *Bot) send(ctx context.Context, chatID int64, text string) {
	b.sendMarkup(ctx, chatID, text, nil)
}

// sendOptions answers with an image and an inline keyboard attached to it.
//
// Telegram puts the image above the caption, so a poster of the top result
// is the preview the user sees before tapping a button. An empty photoURL
// falls back to a plain text message (SendPhoto with an empty URL is a
// client error, not a graceful skip); a SendPhoto error from Telegram
// (image unreachable, etc.) also drops back to text so a missing poster
// never blocks the answer.
func (b *Bot) sendOptions(ctx context.Context, chatID int64, photoURL, caption string,
	markup *models.InlineKeyboardMarkup) {
	if photoURL == "" {
		b.sendMarkup(ctx, chatID, caption, markup)
		return
	}
	params := &tgbot.SendPhotoParams{
		ChatID:  chatID,
		Photo:   &models.InputFileString{Data: photoURL},
		Caption: caption,
	}
	if markup != nil {
		params.ReplyMarkup = markup
	}
	if _, err := b.api.SendPhoto(ctx, params); err != nil {
		b.log.Warn("could not send a photo; falling back to text", "chat", chatID, "error", err)
		b.sendMarkup(ctx, chatID, caption, markup)
	}
}

// sendOptionsAlbum answers a multi-result choice: every result with a poster
// is sent as one Telegram media group (a horizontal strip of thumbnails),
// followed by a separate message carrying the caption and the inline keyboard.
// Two bubbles instead of one is the price of seeing every option at a glance;
// Telegram does not allow images inside keyboard buttons. If no result has a
// poster, the album is skipped and only the button message is sent.
//
// The button message is the one Telegram gives back a message_id for in the
// callback, which is what subsequent edits target — the album is decorative.
func (b *Bot) sendOptionsAlbum(ctx context.Context, chatID int64,
	results []tmdb.SearchResult, caption string, markup *models.InlineKeyboardMarkup) {
	var media []models.InputMedia
	for _, r := range results {
		if r.PosterPath == "" {
			continue
		}
		media = append(media, &models.InputMediaPhoto{
			Media: tmdb.PosterURL(r.PosterPath, "w300"),
		})
	}
	if len(media) > 0 {
		if _, err := b.api.SendMediaGroup(ctx, &tgbot.SendMediaGroupParams{
			ChatID: chatID,
			Media:  media,
		}); err != nil {
			b.log.Warn("could not send a media group; sending only the buttons",
				"chat", chatID, "count", len(media), "error", err)
		}
	}
	b.sendMarkup(ctx, chatID, caption, markup)
}

func (b *Bot) sendMarkup(ctx context.Context, chatID int64, text string,
	markup *models.InlineKeyboardMarkup) {
	params := &tgbot.SendMessageParams{ChatID: chatID, Text: text}
	if markup != nil {
		params.ReplyMarkup = markup
	}

	if _, err := b.api.SendMessage(ctx, params); err != nil {
		b.log.Warn("could not send a message", "chat", chatID, "error", err)
	}
}

// editText replaces the text of the message a button was pressed on.
//
// Editing rather than sending again, because the alternative is a chat that
// grows a new message for every tap: press a series, press a season, and the
// screen fills with the conversation you already had. Editing also removes the
// buttons, which is the point — the choice has been made.
func (b *Bot) editText(ctx context.Context, chatID int64, messageID int, text string) {
	if _, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
	}); err != nil {
		// Telegram refuses to edit a message whose text and markup are both
		// unchanged, and it refuses after a while for any message. Neither is
		// worth failing over: the user gets a new message instead.
		b.log.Debug("could not edit a message", "chat", chatID, "error", err)
		b.send(ctx, chatID, text)
	}
}

func (b *Bot) editMarkup(ctx context.Context, chatID int64, messageID int, text string,
	markup *models.InlineKeyboardMarkup) {
	if _, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:      chatID,
		MessageID:   messageID,
		Text:        text,
		ReplyMarkup: markup,
	}); err != nil {
		b.log.Debug("could not edit a message with buttons", "chat", chatID, "error", err)
		b.sendMarkup(ctx, chatID, text, markup)
	}
}

// messageOf returns the message an update came from, whichever shape it has.
func messageOf(update *models.Update) *models.Message {
	switch {
	case update.Message != nil:
		return update.Message
	case update.CallbackQuery != nil && update.CallbackQuery.Message.Message != nil:
		return update.CallbackQuery.Message.Message
	default:
		return nil
	}
}

// commandArgs returns what follows a command, with the @botname suffix
// removed.
//
// Telegram sends "/list@my_bot" in a group, and treating that as one word
// means the argument is "@my_bot" rather than nothing.
func commandArgs(update *models.Update) string {
	if update.Message == nil {
		return ""
	}

	text := strings.TrimSpace(update.Message.Text)
	parts := strings.SplitN(text, " ", 2)
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// ---------------------------------------------------------------------------
// Describing a request
// ---------------------------------------------------------------------------

// titleWithSeason names a request the way a person does.
func titleWithSeason(r *storage.Request) string {
	title := r.Media.DisplayTitle()
	if r.Media.Year > 0 {
		title += fmt.Sprintf(" (%d)", r.Media.Year)
	}
	if r.Media.Ref.IsSeason() {
		title += fmt.Sprintf(", сезон %d", r.Media.Ref.SeasonNumber())
	}
	return title
}

// stateLine says where a request is, in one line.
func stateLine(r *storage.Request) string {
	switch r.State {
	case storage.StateDownloading:
		out := fmt.Sprintf("качается, %.1f%%", r.Progress*100)
		if r.ETASeconds > 0 {
			out += ", осталось " + humanDuration(time.Duration(r.ETASeconds)*time.Second)
		}
		return out

	case storage.StateSearching:
		return fmt.Sprintf("ищет сейчас, попыток: %d", r.AttemptCount)

	case storage.StateOngoing:
		// The counts are what makes the line useful: how much of the season
		// is already on disk, and how much is still to come.
		if r.SelectedVideoCount > 0 && r.SelectedEpisodeCount > 0 {
			return fmt.Sprintf("ждёт остальные серии: %d из %d",
				r.SelectedVideoCount, r.SelectedEpisodeCount)
		}
		return "ждёт остальные серии"

	case storage.StatePending:
		if r.AttemptCount == 0 {
			return "ждёт первого поиска"
		}
		if r.NextRunAt != nil && r.NextRunAt.After(time.Now()) {
			return fmt.Sprintf("попыток: %d, следующая через %s",
				r.AttemptCount, humanDuration(time.Until(*r.NextRunAt)))
		}
		return fmt.Sprintf("попыток: %d", r.AttemptCount)

	default:
		return string(r.State)
	}
}

// watching reports whether a user is one of a request's watchers.
func watching(watchers []storage.Watcher, userID int64) bool {
	for _, w := range watchers {
		if w.UserID == userID {
			return true
		}
	}
	return false
}

// isExpired reports whether an error is an aged-out search session.
func isExpired(err error) bool {
	return errors.Is(err, storage.ErrSessionExpired)
}

// humanDuration renders an interval the way somebody reads one.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	switch {
	case d < time.Minute:
		return "меньше минуты"
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч", int(d.Hours()))
	default:
		return fmt.Sprintf("%d дн", int(d.Hours()/24))
	}
}

// truncateRunes shortens a string on a rune boundary.
//
// Telegram rejects a button whose text is over its limit, and the titles here
// are Russian: cutting by bytes would split a character and put a replacement
// glyph in the chat.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}

// telegramIDString renders a chat id for storage.
func telegramIDString(id int64) string { return strconv.FormatInt(id, 10) }
