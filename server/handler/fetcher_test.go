// Copyright 2026 Palantir Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package handler

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v81/github"
	"github.com/palantir/go-githubapp/appconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigFetcherCachesSuccessfulConfig(t *testing.T) {
	transport := &configCountingTransport{}
	client := githubClientForTransport(t, transport)
	now := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	fetcher := NewConfigFetcher(appconfig.NewLoader([]string{".policy.yml"}))
	fetcher.Clock = func() time.Time { return now }
	fetcher.CacheTTL = time.Minute

	first := fetcher.ConfigForRepositoryBranch(context.Background(), client, "testowner", "testrepo", "main")
	second := fetcher.ConfigForRepositoryBranch(context.Background(), client, "testowner", "testrepo", "main")

	require.NoError(t, first.LoadError)
	require.NoError(t, second.LoadError)
	require.NotNil(t, first.Config)
	require.NotNil(t, second.Config)
	assert.NotSame(t, first.Config, second.Config, "cached configs should be cloned before reuse")
	assert.Equal(t, 1, transport.requestCount())

	now = now.Add(time.Minute + time.Second)
	third := fetcher.ConfigForRepositoryBranch(context.Background(), client, "testowner", "testrepo", "main")

	require.NoError(t, third.LoadError)
	assert.Equal(t, 2, transport.requestCount())
}

func TestConfigFetcherSingleflightsConcurrentLoads(t *testing.T) {
	transport := &configCountingTransport{delay: 50 * time.Millisecond}
	client := githubClientForTransport(t, transport)
	fetcher := NewConfigFetcher(appconfig.NewLoader([]string{".policy.yml"}))
	fetcher.CacheTTL = time.Minute

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			fc := fetcher.ConfigForRepositoryBranch(context.Background(), client, "testowner", "testrepo", "main")
			errs <- fc.LoadError
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, transport.requestCount())
}

func TestConfigFetcherDoesNotCacheLoadErrors(t *testing.T) {
	transport := &configCountingTransport{status: http.StatusForbidden}
	client := githubClientForTransport(t, transport)
	fetcher := NewConfigFetcher(appconfig.NewLoader([]string{".policy.yml"}))
	fetcher.CacheTTL = time.Minute

	first := fetcher.ConfigForRepositoryBranch(context.Background(), client, "testowner", "testrepo", "main")
	second := fetcher.ConfigForRepositoryBranch(context.Background(), client, "testowner", "testrepo", "main")

	require.Error(t, first.LoadError)
	require.Error(t, second.LoadError)
	assert.Equal(t, 2, transport.requestCount())
}

func TestConfigFetcherBoundsCacheAndEvictsEarliestExpiry(t *testing.T) {
	transport := &configCountingTransport{}
	client := githubClientForTransport(t, transport)
	now := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	fetcher := NewConfigFetcher(appconfig.NewLoader([]string{".policy.yml"}))
	fetcher.Clock = func() time.Time { return now }
	fetcher.CacheTTL = time.Hour
	fetcher.MaxCacheEntries = 2

	for _, repository := range []string{"repo-1", "repo-2", "repo-3"} {
		fc := fetcher.ConfigForRepositoryBranch(context.Background(), client, "owner", repository, "main")
		require.NoError(t, fc.LoadError)
		now = now.Add(time.Second)
	}

	assert.Len(t, fetcher.cache, 2)
	assert.NotContains(t, fetcher.cache, configCacheKey("owner", "repo-1", "main"))

	fc := fetcher.ConfigForRepositoryBranch(context.Background(), client, "owner", "repo-1", "main")
	require.NoError(t, fc.LoadError)
	assert.Equal(t, 4, transport.requestCount(), "the evicted branch should be loaded again")
}

func TestConfigFetcherSweepsExpiredEntries(t *testing.T) {
	transport := &configCountingTransport{}
	client := githubClientForTransport(t, transport)
	now := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	fetcher := NewConfigFetcher(appconfig.NewLoader([]string{".policy.yml"}))
	fetcher.Clock = func() time.Time { return now }
	fetcher.CacheTTL = time.Minute

	for _, repository := range []string{"repo-1", "repo-2"} {
		fc := fetcher.ConfigForRepositoryBranch(context.Background(), client, "owner", repository, "main")
		require.NoError(t, fc.LoadError)
	}
	require.Len(t, fetcher.cache, 2)

	now = now.Add(time.Minute)
	fc := fetcher.ConfigForRepositoryBranch(context.Background(), client, "owner", "repo-3", "main")
	require.NoError(t, fc.LoadError)
	assert.Len(t, fetcher.cache, 1)
	assert.Contains(t, fetcher.cache, configCacheKey("owner", "repo-3", "main"))
}

func TestConfigFetcherPanicCleansUpInflightLoad(t *testing.T) {
	transport := &configPanicOnceTransport{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	client := githubClientForTransport(t, transport)
	fetcher := NewConfigFetcher(appconfig.NewLoader([]string{".policy.yml"}))
	key := configCacheKey("owner", "repo", "main")
	panicked := make(chan struct{})

	go func() {
		defer close(panicked)
		defer func() { _ = recover() }()
		fetcher.ConfigForRepositoryBranch(context.Background(), client, "owner", "repo", "main")
	}()
	<-transport.entered

	fetcher.mu.Lock()
	in := fetcher.inflight[key]
	fetcher.mu.Unlock()
	require.NotNil(t, in)

	close(transport.release)
	select {
	case <-panicked:
	case <-time.After(time.Second):
		t.Fatal("panicking config load did not return")
	}
	select {
	case <-in.done:
	case <-time.After(time.Second):
		t.Fatal("panicking config load did not unblock followers")
	}

	fetcher.mu.Lock()
	assert.NotContains(t, fetcher.inflight, key)
	fetcher.mu.Unlock()

	fc := fetcher.ConfigForRepositoryBranch(context.Background(), client, "owner", "repo", "main")
	require.NoError(t, fc.LoadError)
}

type configCountingTransport struct {
	mu       sync.Mutex
	requests int
	delay    time.Duration
	status   int
}

type configPanicOnceTransport struct {
	mu       sync.Mutex
	panicked bool
	entered  chan struct{}
	release  chan struct{}
}

func (t *configPanicOnceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	panicNow := !t.panicked
	t.panicked = true
	t.mu.Unlock()
	if panicNow {
		close(t.entered)
		<-t.release
		panic("test transport panic")
	}

	content := base64.StdEncoding.EncodeToString([]byte("{}\n"))
	return jsonResponse(req, http.StatusOK, `{"type":"file","encoding":"base64","content":"`+content+`","name":".policy.yml","path":".policy.yml"}`), nil
}

func (t *configCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.delay > 0 {
		time.Sleep(t.delay)
	}

	t.mu.Lock()
	t.requests++
	t.mu.Unlock()

	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		return jsonResponse(req, status, `{"message":"boom"}`), nil
	}

	content := base64.StdEncoding.EncodeToString([]byte("{}\n"))
	return jsonResponse(req, http.StatusOK, `{"type":"file","encoding":"base64","content":"`+content+`","name":".policy.yml","path":".policy.yml"}`), nil
}

func (t *configCountingTransport) requestCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requests
}

func githubClientForTransport(t *testing.T, transport http.RoundTripper) *github.Client {
	t.Helper()

	client := github.NewClient(&http.Client{Transport: transport})
	baseURL, err := url.Parse("http://github.localhost/")
	require.NoError(t, err)
	client.BaseURL = baseURL
	return client
}
