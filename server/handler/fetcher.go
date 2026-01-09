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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/google/go-github/v81/github"
	"github.com/palantir/go-githubapp/appconfig"
	"github.com/palantir/policy-bot/policy"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v2"
)

type FetchedConfig struct {
	Config     *policy.Config
	LoadError  error
	ParseError error

	Source string
	Path   string

	ContentHash string
}

type ConfigFetcher struct {
	Loader *appconfig.Loader
}

func (cf *ConfigFetcher) ConfigForRepositoryBranch(ctx context.Context, client *github.Client, owner, repository, branch string) FetchedConfig {
	retries := 0
	delay := 1 * time.Second
	for {
		start := time.Now()
		c, err := cf.Loader.LoadConfig(ctx, client, owner, repository, branch)
		zerolog.Ctx(ctx).Debug().
			Dur("elapsed", time.Since(start)).
			Err(err).
			Msg("policy_config_load")
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

		fc.ContentHash = configContentHash(c.Content)

		var pc policy.Config
		if err := yaml.UnmarshalStrict(c.Content, &pc); err != nil {
			fc.ParseError = err
		} else {
			fc.Config = &pc
		}
		return fc
	}
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

func configContentHash(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
