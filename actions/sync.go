package actions

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/bitcomplete/plz-cli/client/stack"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/pkg/errors"
	"github.com/shurcooL/graphql"
	"github.com/urfave/cli/v2"
)

type syncUpdatedReview struct {
	HeadBranch         string `graphql:"headBranch"`
	LatestRevisionList struct {
		Revisions []struct {
			Number        int    `graphql:"number"`
			HeadCommitSHA string `graphql:"headCommitSha"`
			BaseBranch    string `graphql:"baseBranch"`
			BaseCommitSHA string `graphql:"baseCommitSha"`
		} `graphql:"revisions"`
	} `graphql:"latestRevisionList: revisionList(options: {count: 1})"`
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

	isClean, err := isCleanWorktree(ctx, gitHubRepo)
	if err != nil {
		return err
	}
	if !isClean {
		return errors.Errorf("index is not clean")
	}

	headRef, err := gitHubRepo.GitRepo().Head()
	if err != nil {
		return errors.WithStack(err)
	}
	headRefName := headRef.Name()
	deps.DebugLog.Printf("HEAD is %v at %v", headRefName, headRef.Hash())
	if !headRefName.IsBranch() {
		return errors.Errorf("HEAD is not a branch")
	}

	repo := gitHubRepo.GitRepo()
	headCommit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return errors.WithStack(err)
	}
	s, err := stack.Load(ctx, repo, graphqlClient, headCommit, gitHubRepo.DefaultBranch())
	if err != nil {
		return err
	}
	if len(s) == 0 {
		return nil
	}

	newBase := ""
	var newHeadRef *plumbing.Reference
	i := len(s) - 1
	for ; i >= 0; i-- {
		ci := s[i]
		status := ci.Status()
		if status == stack.CommitStatusNew || status == stack.CommitStatusModified {
			break
		}
		review := ci.Review
		if review.Status == stack.ReviewStatusDeleted {
			continue
		}
		if review.Status == stack.ReviewStatusMerged {
			if status == stack.CommitStatusCurrent {
				continue
			}
			// The stack is based on an old revision for this merged review.
			// Pull the merged review's base branch to ensure we have the merge
			// commit available locally.
			latestRevision := review.LatestRevision
			err = pullBranch(ctx, repo, latestRevision.BaseBranch)
			if err != nil {
				return err
			}
			// Specify new base commit for the stack starting at merge commit.
			newBase = latestRevision.HeadCommitSHA
			// Don't include merge commit in those to rebase on top of newBase.
			i--
			break
		}
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
		updatedReview := mutation.Review
		updatedLatestRevision := updatedReview.LatestRevisionList.Revisions[0]
		if updatedLatestRevision.HeadCommitSHA == ci.Commit.Hash.String() {
			continue
		}
		deps.DebugLog.Printf("pulling branch for review %v: %v", review.ID, review.HeadBranch)
		err = pullBranch(ctx, repo, review.HeadBranch)
		if err != nil {
			return err
		}

		newHeadRef, err = repo.Reference(plumbing.NewBranchReferenceName(review.HeadBranch), true)
		if err != nil {
			return errors.WithStack(err)
		}
		if newHeadRef.Hash().String() != updatedLatestRevision.HeadCommitSHA {
			return errors.Errorf(
				"branch %v had wrong hash, got %v, want %v",
				review.HeadBranch,
				newHeadRef.Hash(), updatedLatestRevision.HeadCommitSHA,
			)
		}
		newBase = review.HeadBranch
	}

	if newHeadRef != nil {
		deps.DebugLog.Println("repointing", headRefName, "to", newHeadRef.Hash())
		err = gitHubRepo.GitRepo().Storer.SetReference(
			plumbing.NewHashReference(headRefName, newHeadRef.Hash()),
		)
		if err != nil {
			return err
		}
	}

	// Re-point the tip review's branch to what was fetched.
	if i >= 0 && newBase != "" {
		return errors.Errorf(
			"some commits are ahead, run git rebase --onto %s %s~%d",
			newBase,
			headRefName.Short(),
			i+1,
		)
	}
	return nil
}

func pullBranch(ctx context.Context, repo *git.Repository, name string) error {
	deps := deps.FromContext(ctx)
	remote, err := repo.Remote(git.DefaultRemoteName)
	if err != nil {
		return errors.WithStack(err)
	}
	deps.DebugLog.Printf("fetching branch %v", name)
	refSpec := fmt.Sprintf(
		"+refs/heads/%[1]s:refs/remotes/%[2]s/%[1]s",
		name,
		git.DefaultRemoteName,
	)
	err = remote.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(refSpec)},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return errors.WithStack(err)
	}
	remoteRefName := plumbing.NewRemoteReferenceName(git.DefaultRemoteName, name)
	updatedRef, err := repo.Reference(remoteRefName, true)
	if err != nil {
		return errors.WithStack(err)
	}
	deps.DebugLog.Println("repointing", name, "to", updatedRef.Hash())
	localRefName := plumbing.NewBranchReferenceName(name)
	err = repo.Storer.SetReference(plumbing.NewHashReference(localRefName, updatedRef.Hash()))
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}
