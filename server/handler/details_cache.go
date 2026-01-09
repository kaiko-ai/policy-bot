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
	"fmt"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/palantir/policy-bot/policy/common"
)

type DetailsResultCache struct {
	ttl   time.Duration
	cache *lru.Cache
	mu    sync.Mutex
}

type detailsCacheEntry struct {
	result      *common.Result
	err         error
	isTemporary bool
	expiresAt   time.Time
}

func NewDetailsResultCache(size int, ttl time.Duration) (*DetailsResultCache, error) {
	cache, err := lru.New(size)
	if err != nil {
		return nil, err
	}
	return &DetailsResultCache{
		ttl:   ttl,
		cache: cache,
	}, nil
}

func (c *DetailsResultCache) Get(key string) (*detailsCacheEntry, bool) {
	if c == nil {
		return nil, false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if val, ok := c.cache.Get(key); ok {
		entry, ok := val.(*detailsCacheEntry)
		if !ok {
			c.cache.Remove(key)
			return nil, false
		}
		if entry.expiresAt.Before(now) {
			c.cache.Remove(key)
			return nil, false
		}
		return entry, true
	}
	return nil, false
}

func (c *DetailsResultCache) Set(key string, entry *detailsCacheEntry) {
	if c == nil || entry == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache.Add(key, entry)
}

func detailsCacheKey(owner, repo string, number int, headSHA string, config FetchedConfig) string {
	return fmt.Sprintf(
		"%s/%s#%d@%s|%s|%s|%s",
		owner,
		repo,
		number,
		headSHA,
		config.Source,
		config.Path,
		config.ContentHash,
	)
}
