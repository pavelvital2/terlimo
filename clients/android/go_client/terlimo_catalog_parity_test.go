package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type fullCatalogParityClient struct {
	*recoveryClient
	calls int
	raw   []byte
	err   error
}

func (f *fullCatalogParityClient) Catalog(_ context.Context, registration, revision string) ([]byte, error) {
	f.calls++
	if registration != "reg" || revision != "" {
		f.t.Fatal("controller must request existing registration, full snapshot")
	}
	return f.raw, f.err
}

func TestCurrentCatalogBranchParity(t *testing.T) {
	for _, kind := range []string{"full", "read_eof", "not_modified"} {
		t.Run(kind, func(t *testing.T) {
			old := runnerCatalog(time.Now().UTC().Truncate(time.Second))
			c, base, _ := recoveryController(t, old)
			f := &fullCatalogParityClient{recoveryClient: base, raw: recoveryRaw(old, "ok")}
			switch kind {
			case "read_eof":
				f.raw, f.err = nil, io.EOF
			case "not_modified":
				f.raw = []byte(`{"v":1,"status":"not_modified"}`)
			}
			err := c.synchronizeClient(context.Background(), f, false, 3)
			if kind == "full" && err != nil {
				t.Fatal(err)
			}
			if kind == "read_eof" && !errors.Is(err, io.EOF) {
				t.Fatal("read EOF changed", err)
			}
			if kind == "not_modified" && managedCode(err) != "BAD_MESSAGE" {
				t.Fatal("unsolicited not_modified accepted", err)
			}
			if f.calls != 1 || len(base.trace) != 0 || c.saved.Pending != nil || c.saved.Installation != "unchanged-key-fingerprint" {
				t.Fatal("unexpected mutation/retry/identity change")
			}
		})
	}
}
