package actions

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/bitcomplete/plz-cli/client/stack"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-github/v32/github"
	"github.com/pkg/errors"
	"github.com/shurcooL/graphql"
	"github.com/urfave/cli/v2"
)

type reviewInfo struct {
	stack.CommitInfo
	reviewID      string
	pr            *github.PullRequest
	headBranch    string
	baseBranch    string
	updatedCommit *object.Commit
	isUpdated     bool
	reviewer      *github.Reviewers
}

var (
	reviewerUsernameRegex = regexp.MustCompile(
		`^[A-Za-z0-9-]+$`,
	)
)

func Review(c *cli.Context) error {
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

	// Validate reviewer usernames.
	reviewers := c.StringSlice("reviewer")
	for _, reviewer := range reviewers {
		if !reviewerUsernameRegex.MatchString(reviewer) {
			return errors.Errorf("invalid reviewer username: %q", reviewer)
		}
		_, resp, err := gitHubRepo.Client().Users.Get(ctx, reviewer)
		if err != nil {
			if resp.StatusCode == http.StatusNotFound {
				return errors.Errorf("reviewer %q not found", reviewer)
			}
			return errors.WithStack(err)
		}
	}

	headRef, err := gitHubRepo.GitRepo().Head()
	if err != nil {
		return errors.WithStack(err)
	}
	deps.DebugLog.Println("HEAD is at", headRef.Hash())

	ris, err := getReviewInfo(
		ctx,
		gitHubRepo,
		graphqlClient,
		headRef.Hash(),
	)
	if err != nil {
		return err
	}
	numRIs := len(ris)
	if numRIs == 0 {
		return errors.New("no new commits")
	}

	parentHash := ris[0].Commit.ParentHashes[0]
	for i, ri := range ris {
		deps.DebugLog.Println("processing", ri.Commit.Hash)
		commit := ri.Commit
		if ri.pr == nil || parentHash != ri.Commit.ParentHashes[0] {
			deps.DebugLog.Println("commit out of date, creating new commit")
			commit, err = createCommit(gitHubRepo, ri, parentHash)
			if err != nil {
				return err
			}
			ri.updatedCommit = commit
			deps.DebugLog.Println("created new commit", commit.Hash)
		}
		isBranchUpdated, err := updateReviewBranch(ctx, gitHubRepo, ri.headBranch, commit.Hash)
		if err != nil {
			return err
		}
		isPRUpdated, err := createOrUpdatePR(ctx, gitHubRepo, ri, reviewers)
		if err != nil {
			return err
		}
		ri.isUpdated = isBranchUpdated || isPRUpdated
		if i < numRIs-1 && ri.isUpdated {
			// TODO(PLZ-1095): If plz.review processes the webhook for a child
			// review's new revision before the webhook for its parent then the
			// child will be orphaned, and the stack relationship will be
			// broken. This is a hack to reduce the likelihood that a new
			// revision in a child review is processed before a new revision in
			// the parent review. The proper fix is to wait until any revisions
			// that may be created by pushing the branch or updating the PR are
			// created (e.g. by polling the API) before continuing on up the
			// stack.
			time.Sleep(time.Millisecond * 500)
		}
		parentHash = commit.Hash
	}

	printReviewInfo(ctx, ris)

	headRefName := headRef.Name()
	if headRefName.IsBranch() {
		deps.DebugLog.Println("repointing", headRefName, "to", parentHash)
		err := gitHubRepo.GitRepo().Storer.SetReference(
			plumbing.NewHashReference(headRefName, parentHash),
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func isCleanWorktree(ctx context.Context, gitHubRepo *gitHubRepo) (bool, error) {
	// Worktree.Status() is very slow so fall back to the command line instead.
	// https://github.com/go-git/go-git/issues/181
	cmd := exec.Command("git", "status", "--porcelain")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err != nil {
		var exitErr exec.ExitError
		if errors.As(err, &exitErr) {
			return false, errors.Errorf("git status failed: %s", exitErr.Stderr)
		}
		return false, errors.WithStack(err)
	}
	return stdout.Len() == 0, nil
}

func getReviewInfo(
	ctx context.Context,
	gitHubRepo *gitHubRepo,
	graphqlClient *graphql.Client,
	headHash plumbing.Hash,
) ([]*reviewInfo, error) {
	deps := deps.FromContext(ctx)

	repo := gitHubRepo.GitRepo()
	headCommit, err := repo.CommitObject(headHash)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	s, err := stack.Load(ctx, repo, graphqlClient, headCommit, gitHubRepo.DefaultBranch())
	if err != nil {
		return nil, err
	}

	// Collect sequence of commits of interest
	var ris []*reviewInfo
	for _, ci := range s {
		status := ci.Status()
		if status == stack.CommitStatusCurrent && ci.Review.Status == stack.ReviewStatusMerged {
			break
		}
		if status == stack.CommitStatusBehind {
			return nil, errors.Errorf(
				"commit %s for review %s is behind, run plz sync",
				ci.Commit.Hash.String(),
				ci.Review.ID,
			)
		}

		ri, err := makeReviewInfo(ctx, gitHubRepo, ci)
		if err != nil {
			return nil, err
		}
		ris = append(ris, ri)

		statusMessage := "review not found"
		if ri.pr != nil {
			statusMessage = "reviewID: " + ri.Review.ID + " pr: " + ri.pr.GetHTMLURL()
		}
		deps.DebugLog.Println("examined", ci.Commit.Hash, statusMessage)
	}
	// Reverse the array so the tip commit is at the end
	for i, j := 0, len(ris)-1; i < j; i, j = i+1, j-1 {
		ris[i], ris[j] = ris[j], ris[i]
	}

	numNewReviews := 0
	for _, ri := range ris {
		if ri.reviewID == "" {
			numNewReviews++
		}
	}
	var reservedIDs []string
	if numNewReviews != 0 {
		var mutation struct {
			ReserveReviewIDs []string `graphql:"reserveReviewIDs(count: $count)"`
		}
		err := graphqlClient.Mutate(ctx, &mutation, map[string]interface{}{
			"count": graphql.Int(numNewReviews),
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}
		reservedIDs = mutation.ReserveReviewIDs
		deps.DebugLog.Println("reserved review IDs:", reservedIDs)
	}
	baseBranch := gitHubRepo.DefaultBranch()
	for _, ri := range ris {
		if ri.reviewID == "" {
			ri.reviewID, reservedIDs = reservedIDs[0], reservedIDs[1:]
			ri.headBranch = fmt.Sprintf("plz.review/review/%s", ri.reviewID)
		} else {
			ri.headBranch = ri.pr.Head.GetRef()
		}
		ri.baseBranch = baseBranch
		baseBranch = ri.headBranch
	}
	return ris, nil
}

func createCommit(
	gitHubRepo *gitHubRepo,
	ri *reviewInfo,
	parentHash plumbing.Hash,
) (*object.Commit, error) {
	repo := gitHubRepo.GitRepo()
	message := ri.Commit.Message
	if ri.pr == nil {
		message = strings.TrimRightFunc(ri.Commit.Message, unicode.IsSpace) +
			"\n\nplz-review-url: https://plz.review/review/" + ri.reviewID
	}
	newCommit := &object.Commit{
		Author:       ri.Commit.Author,
		Committer:    ri.Commit.Committer,
		Message:      message,
		TreeHash:     ri.Commit.TreeHash,
		ParentHashes: []plumbing.Hash{parentHash},
	}
	obj := repo.Storer.NewEncodedObject()
	if err := newCommit.Encode(obj); err != nil {
		return nil, errors.WithStack(err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	updatedCommit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return updatedCommit, nil
}

func updateReviewBranch(
	ctx context.Context,
	gitHubRepo *gitHubRepo,
	reviewBranch string,
	hash plumbing.Hash,
) (bool, error) {
	deps := deps.FromContext(ctx)
	repo := gitHubRepo.GitRepo()
	isUpdated := false
	// Overwrite the branch
	headRef := "refs/heads/" + reviewBranch
	deps.DebugLog.Println("examining reference", headRef)
	refName := plumbing.ReferenceName(headRef)
	ref, err := repo.Storer.Reference(refName)
	if err != nil && err != plumbing.ErrReferenceNotFound {
		return false, errors.WithStack(err)
	}
	if err == plumbing.ErrReferenceNotFound || ref.Hash() != hash {
		deps.DebugLog.Println("updating reference", headRef, "to", hash)
		err := repo.Storer.SetReference(
			plumbing.NewHashReference(refName, hash),
		)
		if err != nil {
			return false, errors.WithStack(err)
		}
		isUpdated = true
	} else {
		deps.DebugLog.Println("reference already up to date")
	}

	// Push the branch to the remote
	refSpec := fmt.Sprintf("%[1]s:%[1]s", headRef)
	deps.DebugLog.Println("pushing with refspec", refSpec)
	err = repo.PushContext(ctx, &git.PushOptions{
		RemoteName: git.DefaultRemoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(refSpec)},
		Auth:       gitHubRepo.GitAuth(),
		Force:      true,
	})
	if err == git.NoErrAlreadyUpToDate {
		deps.DebugLog.Println("remote reference already up to date")
	} else if err != nil {
		return false, errors.WithStack(err)
	} else {
		isUpdated = true
	}
	return isUpdated, nil
}

func createOrUpdatePR(
	ctx context.Context,
	gitHubRepo *gitHubRepo,
	ri *reviewInfo,
	reviewers []string,
) (bool, error) {
	var prCreatedOrUpdated bool
	deps := deps.FromContext(ctx)
	message := ri.Commit.Message
	if ri.updatedCommit != nil {
		message = ri.updatedCommit.Message
	}
	parts := strings.SplitN(message, "\n", 2)
	title := strings.TrimSpace(parts[0])
	body := ""
	if len(parts) > 1 {
		body = strings.TrimSpace(parts[1])
	}
	var prNumber int
	var reviewersToAdd []string
	if ri.pr == nil {
		deps.DebugLog.Println("creating PR for head branch", ri.headBranch)
		prCreated, _, err := gitHubRepo.Client().PullRequests.Create(
			ctx,
			gitHubRepo.Owner(),
			gitHubRepo.Name(),
			&github.NewPullRequest{
				Head:  &ri.headBranch,
				Base:  &ri.baseBranch,
				Title: &title,
				Body:  &body,
			},
		)
		if err != nil {
			return true, errors.WithStack(err)
		}
		prNumber = prCreated.GetNumber()
		reviewersToAdd = reviewers
		prCreatedOrUpdated = true
	} else {
		prNumber = ri.pr.GetNumber()
		if len(reviewers) > 0 {
			for _, r := range reviewers {
				needToAdd := true
				for _, existingReviewer := range ri.reviewer.Users {
					if existingReviewer.GetLogin() == r {
						needToAdd = false
						break
					}
				}
				if needToAdd {
					reviewersToAdd = append(reviewersToAdd, r)
				}
			}
		}
	}

	if ri.pr != nil && (ri.pr.Base.GetRef() != ri.baseBranch || ri.pr.GetTitle() != title || ri.pr.GetBody() != body) {
		deps.DebugLog.Println("PR", ri.pr.GetHTMLURL(), "is out of date, updating")
		_, _, err := gitHubRepo.Client().PullRequests.Edit(
			ctx,
			gitHubRepo.Owner(),
			gitHubRepo.Name(),
			ri.pr.GetNumber(),
			&github.PullRequest{
				Base: &github.PullRequestBranch{Ref: &ri.baseBranch},
			},
		)
		if err != nil {
			return true, errors.WithStack(err)
		}
		prCreatedOrUpdated = true
	}

	if len(reviewersToAdd) > 0 {
		deps.DebugLog.Println("Adding reviewers ", reviewersToAdd, "to PR", ri.pr.GetHTMLURL())
		_, _, err := gitHubRepo.Client().PullRequests.RequestReviewers(
			ctx,
			gitHubRepo.Owner(),
			gitHubRepo.Name(),
			prNumber,
			github.ReviewersRequest{
				Reviewers: reviewersToAdd,
			},
		)
		if err != nil {
			return true, errors.WithStack(err)
		}
		prCreatedOrUpdated = true
	}

	deps.DebugLog.Println("PR", ri.pr.GetHTMLURL(), "is up to date")
	return prCreatedOrUpdated, nil
}

func printReviewInfo(ctx context.Context, ris []*reviewInfo) {
	deps := deps.FromContext(ctx)
	w := tabwriter.NewWriter(deps.InfoLog.Writer(), 0, 0, 1, ' ', 0)
	for i := len(ris) - 1; i >= 0; i-- {
		ri := ris[i]
		parts := strings.SplitN(ri.Commit.Message, "\n", 2)
		title := strings.TrimSpace(parts[0])
		if len(title) > 47 {
			title = title[:47] + "..."
		}
		reviewURL := "https://plz.review/review/" + ri.reviewID
		status := "unchanged"
		if ri.pr == nil {
			status = "created"
		} else if ri.isUpdated {
			status = "updated"
		}
		commit := ri.Commit
		if ri.updatedCommit != nil {
			commit = ri.updatedCommit
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", commit.Hash.String()[:8], title, status, reviewURL)
	}
	w.Flush()
}

func makeReviewInfo(
	ctx context.Context,
	gitHubRepo *gitHubRepo,
	ci stack.CommitInfo,
) (*reviewInfo, error) {
	ri := &reviewInfo{CommitInfo: ci}
	if ri.Review == nil {
		return ri, nil
	}
	ri.reviewID = ci.Review.ID
	pr, _, err := gitHubRepo.Client().PullRequests.Get(
		ctx,
		gitHubRepo.Owner(),
		gitHubRepo.Name(),
		ci.GitHubPR,
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if pr.GetState() != "open" {
		return nil, errors.New("something's not right, PR is closed")
	}
	ri.pr = pr
	reviewers, _, err := gitHubRepo.Client().PullRequests.ListReviewers(
		ctx,
		gitHubRepo.Owner(),
		gitHubRepo.Name(),
		pr.GetNumber(),
		&github.ListOptions{},
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	ri.reviewer = reviewers
	return ri, nil
}
