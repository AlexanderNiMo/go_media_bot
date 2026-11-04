package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/storage"
)

func TestCompactMessage(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("a", 4095), strings.Repeat("я", 4096),
		strings.Repeat("я", 4097), strings.Repeat("🎬", 2048),
		strings.Repeat("🎬", 2049), strings.Repeat("я🎬", 2000),
	} {
		got := compactMessage(text, 4096)
		if !utf8.ValidString(got) || len(utf16.Encode([]rune(got))) > 4096 {
			t.Fatal("compacted text is invalid or too long")
		}
		if len(utf16.Encode([]rune(text))) <= 4096 && got != text {
			t.Fatal("short message changed")
		}
		if got != text && !strings.HasSuffix(got, "…") {
			t.Fatal("missing truncation marker")
		}
	}
}

func TestNotificationTextCompactsLongMessages(t *testing.T) {
	bot := &Bot{cfg: &config.Config{Server: config.Server{BaseURL: "https://media.example/", BasePath: "/mediad"}}}
	short := storage.PendingTelegram{Kind: storage.NotificationCompleted, Title: "Готово", Body: "Файл на месте"}
	if got := bot.notificationText(short); got != "✅ Готово\n\nФайл на месте" {
		t.Fatal(got)
	}
	for _, p := range []storage.PendingTelegram{
		{Kind: storage.NotificationManual, RequestID: 42, Title: "Выберите релиз", Body: strings.Repeat("Релиз 🎬\n", 1000)},
		{Kind: storage.NotificationManual, RequestID: 42, Title: strings.Repeat("🎬", 5000)},
	} {
		got := bot.notificationText(p)
		if !utf8.ValidString(got) || len(utf16.Encode([]rune(got))) > 4096 {
			t.Fatal("notification too long or invalid")
		}
		if !strings.Contains(got, "Запрос №42") || !strings.HasSuffix(got, "\n\nПолный текст: https://media.example/mediad/notifications") {
			t.Fatal("request number or full text link missing")
		}
	}
}
