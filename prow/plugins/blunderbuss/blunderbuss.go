/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package blunderbuss

import (
	"fmt"
	"math"
	"math/rand"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
)

const (
	pluginName = "blunderbuss"
)

func init() {
	plugins.RegisterPullRequestHandler(pluginName, handlePullRequest, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	var pluralSuffix string
	if config.Blunderbuss.ReviewerCount != 1 {
		pluralSuffix = "s"
	}
	// Omit the fields [WhoCanUse, Usage, Examples] because this plugin is not triggered by human actions.
	return &pluginhelp.PluginHelp{
			Description: "The blunderbuss plugin automatically requests reviews from reviewers when a new PR is created. The reviewers are selected based on the reviewers specified in the OWNERS files that apply to the files modified by the PR.",
			Config: map[string]string{
				"": fmt.Sprintf("Blunderbuss is currently configured to request reviews from %d reviewer%s.", config.Blunderbuss.ReviewerCount, pluralSuffix),
			},
		},
		nil
}

// weightMap is a map of user to a weight for that user.
type weightMap map[string]int64

type ownersClient interface {
	Reviewers(path string) sets.String
	LeafReviewers(path string) sets.String
}

type githubClient interface {
	RequestReview(org, repo string, number int, logins []string) error
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
}

func handlePullRequest(pc plugins.PluginClient, pre github.PullRequestEvent) error {
	if pre.Action != github.PullRequestActionOpened {
		return nil
	}

	oc, err := pc.OwnersClient.LoadRepoOwners(pre.Repo.Owner.Login, pre.Repo.Name)
	if err != nil {
		return fmt.Errorf("error loading RepoOwners: %v", err)
	}

	return handle(pc.GitHubClient, oc, pc.Logger, pc.PluginConfig.Blunderbuss.ReviewerCount, &pre)
}

func handle(ghc githubClient, oc ownersClient, log *logrus.Entry, reviewerCount int, pre *github.PullRequestEvent) error {
	changes, err := ghc.GetPullRequestChanges(pre.Repo.Owner.Login, pre.Repo.Name, pre.Number)
	if err != nil {
		return fmt.Errorf("error getting PR changes: %v", err)
	}

	author := pre.PullRequest.User.Login
	potentialReviewers, weightSum := getPotentialReviewers(oc, author, changes, true)
	reviewers := selectMultipleReviewers(log, potentialReviewers, weightSum, reviewerCount)
	if len(reviewers) < reviewerCount {
		// Didn't find enough leaf reviewers, need to include reviewers from parent OWNERS files.
		potentialReviewers, weightSum := getPotentialReviewers(oc, author, changes, false)
		for _, reviewer := range reviewers {
			delete(potentialReviewers, reviewer)
		}
		reviewers = append(reviewers, selectMultipleReviewers(log, potentialReviewers, weightSum, reviewerCount-len(reviewers))...)
		if missing := reviewerCount - len(reviewers); missing > 0 {
			log.Errorf("Not enough reviewers found in OWNERS files for files touched by this PR. %d/%d reviewers found.", len(reviewers), reviewerCount)
		}
	}
	if len(reviewers) > 0 {
		log.Infof("Requesting reviews from users %s.", reviewers)
		return ghc.RequestReview(pre.Repo.Owner.Login, pre.Repo.Name, pre.Number, reviewers)
	}
	return nil
}

func chance(val, total int64) float64 {
	return 100.0 * float64(val) / float64(total)
}

func getPotentialReviewers(owners ownersClient, author string, files []github.PullRequestChange, leafOnly bool) (weightMap, int64) {
	potentialReviewers := weightMap{}
	weightSum := int64(0)
	var fileOwners sets.String
	for _, file := range files {
		fileWeight := int64(1)
		if file.Changes != 0 {
			fileWeight = int64(file.Changes)
		}
		// Judge file size on a log scale-- effectively this
		// makes three buckets, we shouldn't have many 10k+
		// line changes.
		fileWeight = int64(math.Log10(float64(fileWeight))) + 1
		if leafOnly {
			fileOwners = owners.LeafReviewers(file.Filename)
		} else {
			fileOwners = owners.Reviewers(file.Filename)
		}

		for _, owner := range fileOwners.List() {
			if owner == author {
				continue
			}
			potentialReviewers[owner] = potentialReviewers[owner] + fileWeight
			weightSum += fileWeight
		}
	}
	return potentialReviewers, weightSum
}

func selectMultipleReviewers(log *logrus.Entry, potentialReviewers weightMap, weightSum int64, count int) []string {
	for name, weight := range potentialReviewers {
		log.Debugf("Reviewer %s had chance %02.2f%%", name, chance(weight, weightSum))
	}

	// Make a copy of the map
	pOwners := weightMap{}
	for k, v := range potentialReviewers {
		pOwners[k] = v
	}

	owners := []string{}

	for i := 0; i < count; i++ {
		if len(pOwners) == 0 || weightSum == 0 {
			break
		}
		selection := rand.Int63n(weightSum)
		owner := ""
		for o, w := range pOwners {
			owner = o
			selection -= w
			if selection <= 0 {
				break
			}
		}

		owners = append(owners, owner)
		weightSum -= pOwners[owner]

		// Remove this person from the map.
		delete(pOwners, owner)
	}
	return owners
}
