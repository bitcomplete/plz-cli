package actions

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/bitcomplete/plz-cli/client/stack"
	"github.com/pkg/errors"
	"github.com/shurcooL/graphql"
	"github.com/urfave/cli/v2"
)

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

	repo := gitHubRepo.GitRepo()
	headCommit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return errors.WithStack(err)
	}

	s, err := stack.Load(ctx, repo, graphqlClient, headCommit, gitHubRepo.DefaultBranch())
	if err != nil {
		return err
	}

	isClean, err := isCleanWorktree(ctx, gitHubRepo)
	if err != nil {
		return err
	}
	if !isClean {
		deps.InfoLog.Println("index is not clean")
	}

	w := tabwriter.NewWriter(deps.InfoLog.Writer(), 0, 0, 1, ' ', 0)
	for _, ci := range s {
		printReviewStatus(w, ci)
	}
	w.Flush()

	return nil
}

func printReviewStatus(w io.Writer, ci stack.CommitInfo) {
	const (
		asciiColorReset  = "\033[m"
		asciiColorYellow = "\033[33m"
		asciiColorGreen  = "\033[32m"
		asciiColorRed    = "\033[31m"
		asciiColorCyan   = "\033[36m"
	)
	statusText := ""
	color := ""
	urlSuffix := ""
	switch status := ci.Status(); status {
	case stack.CommitStatusCurrent:
		if ci.Review.Status == stack.ReviewStatusMerged {
			statusText = "merged"
			color = asciiColorCyan
		} else {
			statusText = fmt.Sprintf("rev %d, current", ci.Review.LocalRevision.Number)
			color = asciiColorGreen
		}
	case stack.CommitStatusBehind:
		statusText = fmt.Sprintf("rev %d, behind", ci.Review.LocalRevision.Number)
		color = asciiColorYellow
		urlSuffix = fmt.Sprintf("/revision/%d", ci.Review.LocalRevision.Number)
	default:
		statusText = string(status)
		color = asciiColorRed
	}
	parts := strings.SplitN(ci.Commit.Message, "\n", 2)
	title := strings.TrimSpace(parts[0])
	if len(title) > 47 {
		title = title[:47] + "..."
	}
	reviewURL := ""
	if ci.Review != nil {
		reviewURL = fmt.Sprintf("https://plz.review/review/%s%s", ci.Review.ID, urlSuffix)
	}
	fmt.Fprintf(
		w,
		"%s%s\t%s\t(%s)\t%s%s\n",
		color,
		ci.Commit.Hash.String()[:8],
		title,
		statusText,
		reviewURL,
		asciiColorReset,
	)
}
