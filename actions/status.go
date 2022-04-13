package actions

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pkg/errors"
	"github.com/shurcooL/graphql"
	"github.com/urfave/cli/v2"
)

type statusReview struct {
	ID           string `graphql:"id"`
	RevisionList struct {
		Revisions []struct {
			HeadCommitSHA string `graphql:"headCommitSha"`
		} `graphql:"revisions"`
	} `graphql:"revisionList"`
}

func Status(c *cli.Context) error {
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

	headRef, err := gitHubRepo.GitRepo().Head()
	if err != nil {
		return errors.WithStack(err)
	}
	deps.DebugLog.Println("HEAD is at", headRef.Hash())

	commitRange, err := getCommitRange(ctx, gitHubRepo, headRef.Hash())
	if err != nil {
		return err
	}
	if len(commitRange) == 0 {
		return nil
	}

	isClean, err := isCleanWorktree(ctx, gitHubRepo)
	if err != nil {
		return err
	}
	if !isClean {
		deps.InfoLog.Println("unstaged changes")
	}

	w := tabwriter.NewWriter(deps.InfoLog.Writer(), 0, 0, 1, ' ', 0)
	for _, commit := range commitRange {
		var review *statusReview
		if reviewID := readReviewIDFromCommitMessage(commit.Message); reviewID != "" {
			deps.DebugLog.Println("loading review", reviewID)
			var query struct {
				Review statusReview `graphql:"review(id: $reviewID)"`
			}
			err := graphqlClient.Query(ctx, &query, map[string]interface{}{
				"reviewID": graphql.ID(reviewID),
			})
			if err != nil {
				return errors.WithStack(err)
			}
			review = &query.Review
		}
		printReviewStatus(w, commit, review)
	}
	w.Flush()
	return nil
}

func printReviewStatus(w io.Writer, commit *object.Commit, review *statusReview) {
	status := "new"
	if review != nil {
		revisionIndex := -1
		for i, revision := range review.RevisionList.Revisions {
			if revision.HeadCommitSHA == commit.Hash.String() {
				revisionIndex = i
				break
			}
		}
		switch revisionIndex {
		case -1:
			status = "modified"
		case 0:
			status = "current"
		default:
			status = "outdated"
		}
	}
	parts := strings.SplitN(commit.Message, "\n", 2)
	title := strings.TrimSpace(parts[0])
	if len(title) > 47 {
		title = title[:47] + "..."
	}
	reviewURL := ""
	if review != nil {
		reviewURL = "https://plz.review/review/" + review.ID
	}
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", commit.Hash.String()[:8], title, status, reviewURL)
}
