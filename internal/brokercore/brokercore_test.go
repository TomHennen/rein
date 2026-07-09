package brokercore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// countMint returns a MintFunc handing back tok (or err) and counts calls.
func countMint(tok string, err error) (MintFunc, *int) {
	n := 0
	return func(ctx context.Context) (string, time.Time, error) {
		n++
		if err != nil {
			return "", time.Time{}, err
		}
		return tok, time.Now().Add(time.Hour), nil
	}, &n
}

// memCache is an in-memory ReadCache for tests (what the daemon will use).
type memCache struct {
	mu  sync.Mutex
	tok string
	exp time.Time
}

func (m *memCache) Get(skew time.Duration) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tok == "" || time.Until(m.exp) <= skew {
		return "", false
	}
	return m.tok, true
}
func (m *memCache) Put(tok string, exp time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tok, m.exp = tok, exp
}

func baseCore() Core {
	mr, _ := countMint("read-tok", nil)
	mw, _ := countMint("write-tok", nil)
	return Core{MintRead: mr, MintWrite: mw, MintTimeout: time.Second, Logger: discardLogger()}
}

func TestServe_ReadMintsThenCaches(t *testing.T) {
	mr, reads := countMint("read-tok", nil)
	c := Core{MintRead: mr, MintTimeout: time.Second, Logger: discardLogger(), ReadCache: &memCache{}}

	got := c.Serve(context.Background(), Request{Repo: "o/r"})
	if got.Password != "read-tok" || got.Username != CredentialUsername {
		t.Fatalf("first read = %+v, want read-tok", got)
	}
	got = c.Serve(context.Background(), Request{Repo: "o/r"})
	if got.Password != "read-tok" {
		t.Fatalf("second read = %+v", got)
	}
	if *reads != 1 {
		t.Errorf("read minted %d times, want 1 (second should hit cache)", *reads)
	}
}

func TestServe_WriteApprovedMintsAndRecords(t *testing.T) {
	mw, writes := countMint("write-tok", nil)
	var recorded string
	c := baseCore()
	c.MintWrite = mw
	c.ConfirmWrite = func(string) bool { return true }
	c.RecordWrite = func(tok string, _ time.Time) { recorded = tok }

	got := c.Serve(context.Background(), Request{Repo: "o/r", WriteIntent: true})
	if got.Password != "write-tok" {
		t.Fatalf("write = %+v, want write-tok", got)
	}
	if *writes != 1 {
		t.Errorf("write minted %d times, want 1", *writes)
	}
	if recorded != "write-tok" {
		t.Errorf("RecordWrite got %q, want write-tok", recorded)
	}
}

func TestServe_WriteDeniedReturnsPlaceholderNoMint(t *testing.T) {
	mw, writes := countMint("write-tok", nil)
	c := baseCore()
	c.MintWrite = mw
	c.ConfirmWrite = func(string) bool { return false }

	got := c.Serve(context.Background(), Request{Repo: "o/r", WriteIntent: true})
	if got.Password != PlaceholderRefused {
		t.Fatalf("denied write = %+v, want refused placeholder", got)
	}
	if *writes != 0 {
		t.Errorf("write minted %d times on denial, want 0", *writes)
	}
}

func TestServe_OutOfScopeRefusesNoMint(t *testing.T) {
	mr, reads := countMint("read-tok", nil)
	c := baseCore()
	c.MintRead = mr
	c.InScope = func(repo string) bool { return repo == "allowed/repo" }

	got := c.Serve(context.Background(), Request{Repo: "other/repo"})
	if got.Password != PlaceholderRefused {
		t.Fatalf("out-of-scope = %+v, want refused", got)
	}
	if *reads != 0 {
		t.Errorf("minted %d times out of scope, want 0", *reads)
	}
}

func TestServe_EmptyRepoScope(t *testing.T) {
	inScope := func(string) bool { return false } // would refuse any named repo

	t.Run("refuse", func(t *testing.T) {
		c := baseCore()
		c.InScope = inScope
		c.EmptyPathScope = "refuse"
		got := c.Serve(context.Background(), Request{Repo: ""})
		if got.Password != PlaceholderRefused {
			t.Fatalf("empty+refuse = %+v, want refused", got)
		}
	})
	t.Run("allow (default)", func(t *testing.T) {
		c := baseCore()
		c.InScope = inScope // set, but empty repo + default allow -> proceeds to read
		got := c.Serve(context.Background(), Request{Repo: ""})
		if got.Password != "read-tok" {
			t.Fatalf("empty+allow = %+v, want read-tok", got)
		}
	})
}

func TestServe_MintFailureReturnsPlaceholderAndDiag(t *testing.T) {
	mr, _ := countMint("", errors.New("boom"))
	var diag bytes.Buffer
	c := baseCore()
	c.MintRead = mr
	c.Diag = &diag

	got := c.Serve(context.Background(), Request{Repo: "o/r"})
	if got.Password != PlaceholderMintFailed {
		t.Fatalf("mint fail = %+v, want mint-failed placeholder", got)
	}
	if !strings.Contains(diag.String(), "rein doctor") {
		t.Errorf("expected a diag hint pointing at `rein doctor`, got %q", diag.String())
	}
}

func TestServe_ConfirmWritePanicDenies(t *testing.T) {
	mw, writes := countMint("write-tok", nil)
	c := baseCore()
	c.MintWrite = mw
	c.ConfirmWrite = func(string) bool { panic("buggy prompter") }

	got := c.Serve(context.Background(), Request{Repo: "o/r", WriteIntent: true})
	if got.Password != PlaceholderRefused {
		t.Fatalf("panicking confirm = %+v, want refused (fail closed)", got)
	}
	if *writes != 0 {
		t.Errorf("minted despite confirm panic, want 0 got %d", *writes)
	}
}

func TestServe_RecordWritePanicStillServesToken(t *testing.T) {
	c := baseCore()
	c.ConfirmWrite = func(string) bool { return true }
	c.RecordWrite = func(string, time.Time) { panic("ledger broke") }

	got := c.Serve(context.Background(), Request{Repo: "o/r", WriteIntent: true})
	if got.Password != "write-tok" {
		t.Fatalf("RecordWrite panic must not block the token; got %+v", got)
	}
}

// A daemon/proxy caller might build a Core with a nil mint. The core must
// fail closed to the placeholder (TM-G8), never panic.
func TestServe_NilMintReturnsPlaceholderNotPanic(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		c := Core{Logger: discardLogger()} // MintRead nil
		got := c.Serve(context.Background(), Request{Repo: "o/r"})
		if got.Password != PlaceholderMintFailed {
			t.Fatalf("nil read mint = %+v, want mint-failed placeholder", got)
		}
	})
	t.Run("write", func(t *testing.T) {
		c := Core{Logger: discardLogger(), ConfirmWrite: func(string) bool { return true }} // MintWrite nil
		got := c.Serve(context.Background(), Request{Repo: "o/r", WriteIntent: true})
		if got.Password != PlaceholderMintFailed {
			t.Fatalf("nil write mint = %+v, want mint-failed placeholder", got)
		}
	})
}

func TestRepoFromPath(t *testing.T) {
	cases := map[string]string{
		"owner/repo.git":   "owner/repo",
		"/owner/repo.git":  "owner/repo",
		"owner/repo":       "owner/repo",
		"owner/repo/extra": "owner/repo",
		"owner/repo.git/":  "owner/repo",
		"justone":          "",
		"":                 "",
		"/":                "",
	}
	for in, want := range cases {
		if got := RepoFromPath(in); got != want {
			t.Errorf("RepoFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}
