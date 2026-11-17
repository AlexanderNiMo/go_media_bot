package request

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alex/mediad/internal/storage"
)

func TestWaitDownload(t *testing.T) {
	t.Run("waits for completion", func(t *testing.T) {
		reads, beats := 0, 0
		got, err := waitDownload(context.Background(), CheckDownloadInput{PollInterval: time.Millisecond}, func(context.Context) (storage.DownloadProgress, error) {
			reads++
			p := storage.DownloadProgress{QbitHash: "actual"}
			if reads == 3 {
				p.Progress = 1
			}
			return p, nil
		}, func(float64) { beats++ })
		if err != nil || got.State != DownloadDone || got.Hash != "actual" || reads != 3 || beats != 3 {
			t.Fatalf("got %+v, %v; reads=%d beats=%d", got, err, reads, beats)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := waitDownload(ctx, CheckDownloadInput{PollInterval: time.Hour}, func(context.Context) (storage.DownloadProgress, error) { return storage.DownloadProgress{}, nil }, func(float64) { cancel() })
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("retry keeps expired deadline", func(t *testing.T) {
		in := CheckDownloadInput{PollInterval: time.Hour, Deadline: time.Now().Add(-time.Second)}
		for range 2 {
			got, err := waitDownload(context.Background(), in, func(context.Context) (storage.DownloadProgress, error) {
				return storage.DownloadProgress{QbitHash: "actual"}, nil
			}, func(float64) {})
			if err != nil || got.State != DownloadStalled || !got.Blacklist || got.Hash != "actual" {
				t.Fatalf("%+v %v", got, err)
			}
		}
	})
}
