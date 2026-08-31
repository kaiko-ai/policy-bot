// Copyright 2018 Palantir Technologies, Inc.
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
	"errors"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/go-github/v81/github"
	"github.com/palantir/go-githubapp/appconfig"
	"github.com/palantir/policy-bot/policy"
	"gopkg.in/yaml.v2"
)

type FetchedConfig struct {
	Config     *policy.Config
	LoadError  error
	ParseError error

	Source string
	Path   string
}

type ConfigFetcher struct {
	Loader   *appconfig.Loader
	CacheTTL time.Duration
	// MaxCacheEntries bounds cached repository/branch configurations. Values
	// less than one disable caching.
	MaxCacheEntries int
	Clock           func() time.Time

	mu       sync.Mutex
	cache    map[string]configCacheEntry
	inflight map[string]*configInflight
}

const (
	DefaultConfigCacheTTL        = 30 * time.Second
	DefaultConfigMaxCacheEntries = 64
)

type configCacheEntry struct {
	config    FetchedConfig
	expiresAt time.Time
}

type configInflight struct {
	done   chan struct{}
	config FetchedConfig
}

func NewConfigFetcher(loader *appconfig.Loader) *ConfigFetcher {
	return &ConfigFetcher{
		Loader:          loader,
		CacheTTL:        DefaultConfigCacheTTL,
		MaxCacheEntries: DefaultConfigMaxCacheEntries,
	}
}

func (cf *ConfigFetcher) ConfigForRepositoryBranch(ctx context.Context, client *github.Client, owner, repository, branch string) FetchedConfig {
	if cf.CacheTTL <= 0 || cf.MaxCacheEntries <= 0 {
		return cf.loadConfigForRepositoryBranch(ctx, client, owner, repository, branch)
	}

	key := configCacheKey(owner, repository, branch)
	now := cf.now()

	cf.mu.Lock()
	if cf.cache != nil {
		cf.deleteExpiredLocked(now)
		if entry, ok := cf.cache[key]; ok {
			cf.mu.Unlock()
			return cloneFetchedConfig(entry.config)
		}
	}
	if cf.inflight != nil {
		if in := cf.inflight[key]; in != nil {
			cf.mu.Unlock()
			select {
			case <-ctx.Done():
				return FetchedConfig{
					Source:    owner + "/" + repository + "@" + branch,
					LoadError: ctx.Err(),
				}
			case <-in.done:
				return cloneFetchedConfig(in.config)
			}
		}
	}
	if cf.inflight == nil {
		cf.inflight = make(map[string]*configInflight)
	}
	in := &configInflight{done: make(chan struct{})}
	cf.inflight[key] = in
	cf.mu.Unlock()
	defer close(in.done)
	defer func() {
		cf.mu.Lock()
		delete(cf.inflight, key)
		cf.mu.Unlock()
	}()

	fc := cf.loadConfigForRepositoryBranch(ctx, client, owner, repository, branch)

	cf.mu.Lock()
	in.config = cloneFetchedConfig(fc)
	if shouldCacheFetchedConfig(fc) {
		if cf.cache == nil {
			cf.cache = make(map[string]configCacheEntry)
		}
		cf.deleteExpiredLocked(cf.now())
		if _, exists := cf.cache[key]; !exists && len(cf.cache) >= cf.MaxCacheEntries {
			cf.deleteEarliestExpiringLocked()
		}
		cf.cache[key] = configCacheEntry{
			config:    cloneFetchedConfig(fc),
			expiresAt: cf.now().Add(cf.CacheTTL),
		}
	}
	cf.mu.Unlock()

	return fc
}

func (cf *ConfigFetcher) deleteExpiredLocked(now time.Time) {
	for key, entry := range cf.cache {
		if !now.Before(entry.expiresAt) {
			delete(cf.cache, key)
		}
	}
}

func (cf *ConfigFetcher) deleteEarliestExpiringLocked() {
	var earliestKey string
	var earliest time.Time
	for key, entry := range cf.cache {
		if earliestKey == "" || entry.expiresAt.Before(earliest) {
			earliestKey = key
			earliest = entry.expiresAt
		}
	}
	if earliestKey != "" {
		delete(cf.cache, earliestKey)
	}
}

func (cf *ConfigFetcher) loadConfigForRepositoryBranch(ctx context.Context, client *github.Client, owner, repository, branch string) FetchedConfig {
	retries := 0
	delay := 1 * time.Second
	for {
		c, err := cf.Loader.LoadConfig(ctx, client, owner, repository, branch)
		fc := FetchedConfig{
			Source: c.Source,
			Path:   c.Path,
		}

		if err != nil {
			if !os.IsTimeout(err) && !isServerError(err) {
				fc.LoadError = err
				return fc
			}

			retries++
			if retries > 3 {
				fc.LoadError = err
				return fc
			}

			select {
			case <-ctx.Done():
				fc.LoadError = ctx.Err()
				return fc
			case <-time.After(delay):
				delay *= 2
				continue
			}
		}

		if c.IsUndefined() {
			return fc
		}

		var pc policy.Config
		if err := yaml.UnmarshalStrict(c.Content, &pc); err != nil {
			fc.ParseError = err
		} else {
			fc.Config = &pc
		}
		return fc
	}
}

func (cf *ConfigFetcher) now() time.Time {
	if cf.Clock != nil {
		return cf.Clock()
	}
	return time.Now()
}

func configCacheKey(owner, repository, branch string) string {
	return owner + "/" + repository + "@" + branch
}

func shouldCacheFetchedConfig(fc FetchedConfig) bool {
	return fc.LoadError == nil && fc.ParseError == nil
}

func cloneFetchedConfig(fc FetchedConfig) FetchedConfig {
	clone := fc
	if fc.Config == nil {
		return clone
	}

	b, err := yaml.Marshal(fc.Config)
	if err != nil {
		return clone
	}
	var pc policy.Config
	if err := yaml.UnmarshalStrict(b, &pc); err != nil {
		return clone
	}
	clone.Config = &pc
	return clone
}

func isServerError(err error) bool {
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) {
		switch ghErr.Response.StatusCode {
		case http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}
