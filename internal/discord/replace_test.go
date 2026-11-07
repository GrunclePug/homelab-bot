package discord

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/grunclepug/homelab-bot/internal/arr"
	"github.com/grunclepug/homelab-bot/internal/replace"
	"github.com/grunclepug/homelab-bot/internal/seerr"
)

// fakeSonarr implements replace.Sonarr for the steps Run takes.
type fakeSonarr struct {
	replace.Sonarr
	deleteErr error
	searched  []int
}

func (f *fakeSonarr) History(context.Context, int) ([]arr.HistoryRecord, error) { return nil, nil }
func (f *fakeSonarr) DeleteEpisodeFile(context.Context, int) error              { return f.deleteErr }
func (f *fakeSonarr) SearchEpisodes(_ context.Context, ids []int) error {
	f.searched = append(f.searched, ids...)
	return nil
}

// fakeSeerrAPI records comments.
type fakeSeerrAPI struct {
	seerrAPI
	comments []string
}

func (f *fakeSeerrAPI) Comment(_ context.Context, _ int, msg string) error {
	f.comments = append(f.comments, msg)
	return nil
}

func TestFinishReplace(t *testing.T) {
	ctx := context.Background()
	r, fd, _ := newTestRelay(t)
	if err := r.handle(ctx, payload(seerr.NotifyIssueCreated, "7")); err != nil {
		t.Fatal(err)
	}
	post := fd.last()
	so := &fakeSonarr{}
	sc := &fakeSeerrAPI{}
	b := &Bot{relay: r, replace: replace.Service{Sonarr: so}, seerr: sc, pending: newPendingStore(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p := &pendingReplace{IssueID: 7, Chosen: []replace.Target{
		{Kind: replace.Episode, ItemID: 1, FileID: 10, Label: "S02E03 · Three"},
	}}

	text := b.finishReplace(ctx, p, "Ann*")
	if !strings.HasPrefix(text, "**Replacement started.**") {
		t.Errorf("reply = %q", text)
	}
	e := fd.msgs[post.ID].Embeds[0]
	if got := field(e, fieldReplace); !strings.HasPrefix(got, `Replacement started by Ann\*`) || !strings.Contains(got, "S02E03 · Three") {
		t.Errorf("post field = %q", got)
	}
	if ids := customIDs(fd.msgs[post.ID].Components); len(ids) != 2 {
		t.Errorf("open post lost its buttons: %v", ids)
	}
	if len(sc.comments) != 1 || !strings.Contains(sc.comments[0], "S02E03 · Three") || !slices.Equal(so.searched, []int{1}) {
		t.Errorf("comments %q, searched %v", sc.comments, so.searched)
	}

	// Once resolved, a later report keeps the post without buttons, and a
	// replacement that deleted nothing doesn't comment in Seerr.
	if err := r.handle(ctx, payload(seerr.NotifyIssueResolved, "7")); err != nil {
		t.Fatal(err)
	}
	so.deleteErr = errors.New("boom")
	text = b.finishReplace(ctx, p, "Ann")
	if !strings.HasPrefix(text, "**Nothing was replaced.**") {
		t.Errorf("reply = %q", text)
	}
	if ids := customIDs(fd.msgs[post.ID].Components); len(ids) != 0 {
		t.Errorf("resolved post got buttons back: %v", ids)
	}
	if len(sc.comments) != 1 {
		t.Errorf("commented although nothing was deleted: %q", sc.comments)
	}
}

func TestReplaceRunningGuard(t *testing.T) {
	s := newPendingStore()
	if !s.start(7) {
		t.Fatal("first start refused")
	}
	if s.start(7) {
		t.Error("second replacement allowed on the same issue")
	}
	if !s.start(8) {
		t.Error("another issue blocked")
	}
	s.finish(7)
	if !s.start(7) {
		t.Error("start refused after finish")
	}
}
