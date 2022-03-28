package actions

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/pkg/errors"
	"github.com/shurcooL/graphql"
	"github.com/urfave/cli/v2"
)

type syncReview struct {
	ID           string `graphql:"id"`
	Status       string `graphql:"status"`
	HeadBranch   string `graphql:"headBranch"`
	RevisionList struct {
		Revisions []struct {
			Parent *struct {
				Number int `graphql:"number"`
				Review struct {
					ID string `graphql:"id"`
				} `graphql:"review"`
			} `graphql:"parent"`
		} `graphql:"revisions"`
	} `graphql:"revisionList(options: {count: 1})"`
}

type syncUpdatedReview struct {
	HeadBranch   string `graphql:"headBranch"`
	RevisionList struct {
		Revisions []struct {
			Number        int    `graphql:"number"`
			HeadCommitSHA string `graphql:"headCommitSha"`
			BaseBranch    string `graphql:"baseBranch"`
			BaseCommitSHA string `graphql:"baseCommitSha"`
		} `graphql:"revisions"`
	} `graphql:"revisionList(options: {count: 1})"`
}

func loadSyncReview(ctx context.Context, graphqlClient *graphql.Client, reviewID string) (*syncReview, error) {
	var query struct {
		Review syncReview `graphql:"review(id: $reviewID)"`
	}
	err := graphqlClient.Query(ctx, &query, map[string]interface{}{
		"reviewID": graphql.ID(reviewID),
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &query.Review, nil
}

func Sync(c *cli.Context) error {
	ctx := c.Context
	deps := deps.FromContext(ctx)

	token, err := deps.Auth.Token()
	if err != nil {
		return err
	}
	gitHubRepo, err := newGitHubRepo(ctx, token)
	if err != nil {
		return err
	}
	graphqlClient := graphql.NewClient(deps.PlzAPIBaseURL+"/api/v1", &http.Client{
		Transport: &authTransport{Token: token},
	})

	if err := checkCleanWorktree(ctx, gitHubRepo); err != nil {
		return err
	}

	headRef, err := gitHubRepo.GitRepo().Head()
	if err != nil {
		return errors.WithStack(err)
	}
	deps.DebugLog.Println("HEAD is at", headRef.Hash())
	repo := gitHubRepo.GitRepo()
	headCommit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return errors.WithStack(err)
	}
	reviewID := readReviewIDFromCommitMessage(headCommit.Message)
	if reviewID == "" {
		return errors.New("HEAD commit has no review")
	}
	var reviews []*syncReview
	for reviewID != "" {
		deps.DebugLog.Println("loading review", reviewID)
		review, err := loadSyncReview(ctx, graphqlClient, reviewID)
		if err != nil {
			return err
		}
		if review.Status != "open" {
			break
		}
		reviews = append(reviews, review)
		parent := review.RevisionList.Revisions[0].Parent
		if parent == nil {
			break
		}
		reviewID = parent.Review.ID
	}

	remote, err := repo.Remote(git.DefaultRemoteName)
	if err != nil {
		return errors.WithStack(err)
	}
	updatedReviews := make([]*syncUpdatedReview, len(reviews))
	for i := len(reviews) - 1; i >= 0; i-- {
		review := reviews[i]
		var mutation struct {
			Review syncUpdatedReview `graphql:"syncReviewWithParent(reviewID: $reviewID)"`
		}
		deps.DebugLog.Println("syncing review", review.ID)
		err = graphqlClient.Mutate(ctx, &mutation, map[string]interface{}{
			"reviewID": graphql.ID(review.ID),
		})
		if err != nil {
			return errors.WithStack(err)
		}
		updatedReviews[i] = &mutation.Review

		refName := plumbing.NewRemoteReferenceName(git.DefaultRemoteName, review.HeadBranch)
		ref, err := repo.Reference(refName, true)
		if err != nil {
			return errors.WithStack(err)
		}
		refHash := ref.Hash().String()
		newHeadCommitSHA := mutation.Review.RevisionList.Revisions[0].HeadCommitSHA
		if newHeadCommitSHA != refHash {
			deps.DebugLog.Printf("fetching out of date branch %v for review %v", review.HeadBranch, review.ID)
			err = remote.FetchContext(ctx, &git.FetchOptions{
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf("+refs/heads/%[1]s:refs/remotes/%[2]s/%[1]s", review.HeadBranch, git.DefaultRemoteName)),
				},
			})
			if err != nil {
				return errors.WithStack(err)
			}
			// Re-read the reference after fetching so it's up to date.
			ref, err = repo.Reference(refName, true)
			if err != nil {
				return errors.WithStack(err)
			}
			if newHeadCommitSHA != ref.Hash().String() {
				return errors.Errorf("fetched head commit does not match expected for review %v", review.ID)
			}

			if i == 0 {
				// Re-point the tip review's branch to what was fetched.
				headRefName := headRef.Name()
				if headRefName.IsBranch() {
					deps.DebugLog.Println("repointing", headRefName, "to", ref.Hash())
					err := gitHubRepo.GitRepo().Storer.SetReference(
						plumbing.NewHashReference(headRefName, ref.Hash()),
					)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}
