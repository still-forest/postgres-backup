package objstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Store for tests.
type Fake struct {
	mu      sync.Mutex
	objects map[string]fakeObject

	// ListErr, UploadErr, DeleteErr force the corresponding call to fail.
	ListErr   error
	UploadErr error
	DeleteErr error

	// Now supplies LastModified for uploads; defaults to time.Now.
	Now func() time.Time

	Deleted []string
}

type fakeObject struct {
	body []byte
	meta map[string]string
	mod  time.Time
}

// NewFake returns an empty Fake.
func NewFake() *Fake { return &Fake{objects: map[string]fakeObject{}} }

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now().UTC()
}

// Seed inserts an object directly, bypassing UploadErr.
func (f *Fake) Seed(key string, size int64, mod time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = fakeObject{body: make([]byte, size), meta: map[string]string{}, mod: mod}
}

func (f *Fake) Upload(_ context.Context, key string, r io.Reader, meta map[string]string) error {
	if f.UploadErr != nil {
		return f.UploadErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]string, len(meta))
	for k, v := range meta {
		cp[k] = v
	}
	f.objects[key] = fakeObject{body: b, meta: cp, mod: f.now()}
	return nil
}

func (f *Fake) List(_ context.Context, prefix string) ([]Object, error) {
	if f.ListErr != nil {
		return nil, f.ListErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Object
	for k, o := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Object{Key: k, Size: int64(len(o.body)), LastModified: o.mod})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *Fake) Download(_ context.Context, key string, w io.Writer) error {
	f.mu.Lock()
	o, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("objstore: no such key %q", key)
	}
	_, err := io.Copy(w, bytes.NewReader(o.body))
	return err
}

func (f *Fake) Head(_ context.Context, key string) (Object, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return Object{}, nil, fmt.Errorf("objstore: no such key %q", key)
	}
	return Object{Key: key, Size: int64(len(o.body)), LastModified: o.mod}, o.meta, nil
}

func (f *Fake) Delete(_ context.Context, key string) error {
	if f.DeleteErr != nil {
		return f.DeleteErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	f.Deleted = append(f.Deleted, key)
	return nil
}

// Keys returns every stored key, sorted.
func (f *Fake) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
