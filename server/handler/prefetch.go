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
	"sync"
	"time"

	"github.com/palantir/policy-bot/policy"
	"github.com/palantir/policy-bot/policy/approval"
	"github.com/palantir/policy-bot/policy/common"
	"github.com/palantir/policy-bot/policy/predicate"
	"github.com/palantir/policy-bot/pull"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

type prefetchPlan struct {
	needsPagedData    bool
	needsCommits      bool
	needsBody         bool
	needsChangedFiles bool
	needsStatuses     bool
	needsWorkflowRuns bool
	needsLabels       bool
	needsCustomProps  bool
	needsPushedAt     bool
}

type prefetchStats struct {
	ops     int
	errors  int
	errOps  []string
	elapsed time.Duration
}

func (p prefetchPlan) enabled() bool {
	return p.needsPagedData ||
		p.needsBody ||
		p.needsChangedFiles ||
		p.needsStatuses ||
		p.needsWorkflowRuns ||
		p.needsLabels ||
		p.needsCustomProps ||
		p.needsPushedAt
}

func buildPrefetchPlan(cfg *policy.Config) prefetchPlan {
	var plan prefetchPlan
	if cfg == nil {
		return plan
	}

	ruleNames, err := approvalRuleNames(cfg.Policy.Approval)
	if err != nil {
		return plan
	}
	if ruleNames != nil {
		for _, rule := range cfg.ApprovalRules {
			if _, ok := ruleNames[rule.Name]; !ok {
				continue
			}
			plan.applyRule(rule)
		}
	}

	if cfg.Policy.Disapproval != nil {
		plan.applyPredicates(cfg.Policy.Disapproval.Predicates)
		if !cfg.Policy.Disapproval.Requires.IsZero() {
			plan.applyMethods(cfg.Policy.Disapproval.Options.GetDisapproveMethods())
			plan.applyMethods(cfg.Policy.Disapproval.Options.GetRevokeMethods())
		}
	}

	if plan.needsCommits {
		plan.needsPagedData = true
	}

	return plan
}

func (p *prefetchPlan) applyRule(rule *approval.Rule) {
	p.applyPredicates(rule.Predicates)
	p.applyPredicates(rule.Requires.Conditions)
	p.applyMethods(rule.Options.GetMethods())
	p.applyOptions(rule.Options)
}

func (p *prefetchPlan) applyOptions(opts approval.Options) {
	if opts.IsInvalidateOnPush() {
		p.needsCommits = true
		p.needsPushedAt = true
	}
	if opts.IsIgnoreUpdateMerges() {
		p.needsCommits = true
	}
	if !opts.IsAllowContributor() && !opts.IsAllowNonAuthorContributor() {
		p.needsCommits = true
	}
	ignoreCommitsBy := opts.GetIgnoreCommitsBy()
	if !ignoreCommitsBy.IsZero() {
		p.needsCommits = true
	}
}

func (p *prefetchPlan) applyMethods(methods *common.Methods) {
	if methods == nil {
		return
	}
	if len(methods.GetComments()) > 0 || len(methods.GetCommentPatterns()) > 0 {
		p.needsPagedData = true
	}
	if methods.IsGithubReview() || len(methods.GetGithubReviewCommentPatterns()) > 0 {
		p.needsPagedData = true
	}
	if len(methods.GetBodyPatterns()) > 0 {
		p.needsBody = true
	}
}

func (p *prefetchPlan) applyPredicates(predicates predicate.Predicates) {
	for _, pred := range predicates.Predicates() {
		p.applyPredicate(pred)
	}
}

func (p *prefetchPlan) applyPredicate(pred predicate.Predicate) {
	switch pred.(type) {
	case *predicate.ChangedFiles,
		*predicate.OnlyChangedFiles,
		*predicate.NoChangedFiles,
		*predicate.FileAdded,
		*predicate.FileNotAdded,
		*predicate.FileDeleted,
		*predicate.FileNotDeleted,
		*predicate.ModifiedLines:
		p.needsChangedFiles = true
	case predicate.HasLabels, *predicate.HasLabels:
		p.needsLabels = true
	case predicate.HasStatus, *predicate.HasStatus, predicate.HasSuccessfulStatus, *predicate.HasSuccessfulStatus:
		p.needsStatuses = true
	case predicate.HasWorkflowResult, *predicate.HasWorkflowResult:
		p.needsWorkflowRuns = true
	case predicate.CustomPropertyIsNotNull, *predicate.CustomPropertyIsNotNull,
		predicate.CustomPropertyIsNull, *predicate.CustomPropertyIsNull,
		predicate.CustomPropertyMatchesAnyOf, *predicate.CustomPropertyMatchesAnyOf,
		predicate.CustomPropertyMatchesNoneOf, *predicate.CustomPropertyMatchesNoneOf:
		p.needsCustomProps = true
	case *predicate.HasContributorIn,
		*predicate.OnlyHasContributorsIn,
		*predicate.HasValidSignaturesBy,
		*predicate.HasValidSignaturesByKeys:
		p.needsCommits = true
	case predicate.HasValidSignatures, *predicate.HasValidSignatures,
		predicate.AuthorIsOnlyContributor, *predicate.AuthorIsOnlyContributor:
		p.needsCommits = true
	}
}

func (p prefetchPlan) prefetch(ctx context.Context, prctx pull.Context, logger zerolog.Logger) prefetchStats {
	if !p.enabled() || ctx.Err() != nil {
		return prefetchStats{}
	}

	start := time.Now()
	stats := prefetchStats{}

	var wg sync.WaitGroup
	var mu sync.Mutex

	run := func(name string, fn func() error) {
		stats.ops++
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			if err := fn(); err != nil {
				mu.Lock()
				stats.errors++
				stats.errOps = append(stats.errOps, name)
				mu.Unlock()
				logger.Debug().Err(err).Str("op", name).Msg("details_prefetch_error")
			}
		}()
	}

	if p.needsCommits {
		run("commits", func() error {
			_, err := prctx.Commits()
			return err
		})
	} else if p.needsPagedData {
		run("comments", func() error {
			_, err := prctx.Comments()
			return err
		})
	}
	if p.needsBody {
		run("body", func() error {
			_, err := prctx.Body()
			return err
		})
	}
	if p.needsChangedFiles {
		run("changed_files", func() error {
			_, err := prctx.ChangedFiles()
			return err
		})
	}
	if p.needsStatuses {
		run("statuses", func() error {
			_, err := prctx.LatestStatuses()
			return err
		})
	}
	if p.needsWorkflowRuns {
		run("workflow_runs", func() error {
			_, err := prctx.LatestWorkflowRuns()
			return err
		})
	}
	if p.needsLabels {
		run("labels", func() error {
			_, err := prctx.Labels()
			return err
		})
	}
	if p.needsCustomProps {
		run("custom_properties", func() error {
			_, err := prctx.RepositoryCustomProperties()
			return err
		})
	}
	if p.needsPushedAt {
		headSHA := prctx.HeadSHA()
		if headSHA != "" {
			run("pushed_at", func() error {
				_, err := prctx.PushedAt(headSHA)
				return err
			})
		}
	}

	wg.Wait()
	stats.elapsed = time.Since(start)

	logger.Debug().
		Dur("elapsed", stats.elapsed).
		Int("operations", stats.ops).
		Int("errors", stats.errors).
		Strs("error_ops", stats.errOps).
		Msg("details_prefetch")

	return stats
}

func approvalRuleNames(policy approval.Policy) (map[string]struct{}, error) {
	if len(policy) == 0 {
		return nil, nil
	}

	names := make(map[string]struct{})
	root := map[interface{}]interface{}{
		"and": []interface{}(policy),
	}
	if err := collectPolicyRuleNames(root, names, 0); err != nil {
		return nil, err
	}
	return names, nil
}

func collectPolicyRuleNames(policy interface{}, names map[string]struct{}, depth int) error {
	if depth > 10 {
		return errors.Errorf("reached maximum recursive depth while processing policy")
	}

	if ruleName, ok := policy.(string); ok {
		names[ruleName] = struct{}{}
		return nil
	}

	conjunction, ok := policy.(map[interface{}]interface{})
	if !ok {
		return errors.Errorf("malformed policy, expected string or map, but encountered %T", policy)
	}

	if len(conjunction) != 1 {
		return errors.Errorf("malformed policy, expected a single conjunction key, got %d", len(conjunction))
	}

	for _, raw := range conjunction {
		values, ok := raw.([]interface{})
		if !ok {
			return errors.Errorf("expected list of subconditions, but got %T", raw)
		}
		if len(values) == 0 {
			return errors.Errorf("empty list of subconditions is not allowed")
		}

		for _, subpolicy := range values {
			if err := collectPolicyRuleNames(subpolicy, names, depth+1); err != nil {
				return err
			}
		}
	}

	return nil
}
