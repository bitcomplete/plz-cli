package actions

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/user"
	"path"
	"regexp"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gitConfig "github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-github/v32/github"
	"github.com/pkg/errors"
	"github.com/shibumi/go-pathspec"
	"github.com/shurcooL/graphql"
	"github.com/urfave/cli/v2"
)

type reviewInfo struct {
	commit        *object.Commit
	reviewID      string
	pr            *github.PullRequest
	headBranch    string
	baseBranch    string
	updatedCommit *object.Commit
	isUpdated     bool
	reviewer      *github.Reviewers
}

var reviewTrailerRegex = regexp.MustCompile(
	`^\s*((?i)plz-review-url)\s*:\s+https://plz.review/review/(\w+)\s*$`,
)

func Review(c *cli.Context) error {
	ctx := c.Context
	deps := deps.FromContext(ctx)

	if deps.AuthToken == "" {
		return errors.New("error loading GitHub credentials, run plz auth")
	}
	gitHubRepo, err := newGitHubRepo(ctx, deps.AuthToken)
	if err != nil {
		return err
	}
	graphqlClient := graphql.NewClient(deps.PlzAPIURL, &http.Client{
		Transport: &authTransport{Token: deps.AuthToken},
	})

	if err := checkCleanWorktree(ctx, gitHubRepo); err != nil {
		return err
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

	parentHash := ris[0].commit.ParentHashes[0]
	for _, ri := range ris {
		deps.DebugLog.Println("processing", ri.commit.Hash)
		commit := ri.commit
		if ri.pr == nil || parentHash != ri.commit.ParentHashes[0] {
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
		isPRUpdated, err := createOrUpdatePR(ctx, gitHubRepo, ri, c.StringSlice("reviewer"))
		if err != nil {
			return err
		}
		ri.isUpdated = isBranchUpdated || isPRUpdated
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

func checkCleanWorktree(ctx context.Context, gitHubRepo *gitHubRepo) error {
	worktree, err := gitHubRepo.GitRepo().Worktree()
	if err != nil {
		return errors.WithStack(err)
	}
	status, err := worktree.Status()
	if err != nil {
		return errors.WithStack(err)
	}
	if !status.IsClean() {
		// go-git's handling of gitignore is not reliable, so we iterate over
		// un-clean files again to make sure that files to be ignored are
		// properly ignored based on the new ignore patterns.
		ignorePatterns, err := getGitIgnorePatterns()
		if err != nil {
			return err
		}
		for filePath := range status {
			match, err := pathspec.GitIgnore(ignorePatterns, filePath)
			if err != nil {
				return err
			}
			if match {
				delete(status, filePath)
			}
		}
		if len(status) > 0 {
			return errors.Errorf("index is not clean:\n%v", status)
		}
	}
	return nil
}

// getGitIgnorePatterns returns all patterns to ignore for a given project,
// by looking at the project's gitignore, global gitignore, and system gitignore
func getGitIgnorePatterns() ([]string, error) {
	var ignorePatterns []string
	projectIgnorePatterns, err := getIgnorePatterns("./.gitignore")
	if err != nil {
		return nil, err
	}
	ignorePatterns = append(ignorePatterns, projectIgnorePatterns...)
	currentUser, err := user.Current()
	if err != nil {
		return nil, err
	}
	globalIgnorePath, err := getIgnoreFilePathFromConfig(path.Join(currentUser.HomeDir, ".gitconfig"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	globalIgnorePatterns, err := getIgnorePatterns(globalIgnorePath)
	if err != nil {
		return nil, err
	}
	ignorePatterns = append(ignorePatterns, globalIgnorePatterns...)
	systemIgnorePath, err := getIgnoreFilePathFromConfig("/etc/gitconfig")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	systemIgnorePatterns, err := getIgnorePatterns(systemIgnorePath)
	if err != nil {
		return nil, err
	}
	ignorePatterns = append(ignorePatterns, systemIgnorePatterns...)
	var allPatterns []string
	for _, pattern := range ignorePatterns {
		allPatterns = append(allPatterns, pattern)
		// for any pattern that has no wildcard and slash, add a slash at the end
		// to make sure that a new rule is added that every file in a directory
		// is ignored. this is to handle go-git's behaviour where certain files
		// are not ignored (e.g., node_modules/dist/.gitkeep is not ignored even
		// when there is a pattern `node_modules` in gitignore file.
		if !strings.Contains(pattern, "**") &&
			!strings.HasPrefix(pattern, "/") &&
			!strings.HasSuffix(pattern, "/") {
			allPatterns = append(allPatterns, pattern, pattern+"/")
		}
	}
	return allPatterns, nil
}

// getIgnoreFilePathFromConfig returns path to a gitignore file,
// specified given config file
func getIgnoreFilePathFromConfig(configPath string) (string, error) {
	configContent, err := os.Open(configPath)
	if err != nil {
		return "", err
	}
	decoder := gitConfig.NewDecoder(configContent)
	raw := gitConfig.New()
	if err = decoder.Decode(raw); err != nil {
		return "", err
	}
	configSection := raw.Section("core")
	return configSection.Options.Get("excludesfile"), nil
}

// getIgnorePatterns reads a gitignore file and returns its list of patterns
// in a slice of strings
func getIgnorePatterns(filePath string) ([]string, error) {
	content, err := ioutil.ReadFile(filePath)
	if err != nil && !os.IsNotExist(err) {
		return []string{}, err
	}
	var patterns []string
	for _, pattern := range strings.Split(string(content), "\n") {
		if pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	return patterns, nil
}

func getReviewInfo(
	ctx context.Context,
	gitHubRepo *gitHubRepo,
	graphqlClient *graphql.Client,
	headHash plumbing.Hash,
) ([]*reviewInfo, error) {
	deps := deps.FromContext(ctx)

	// Determine the common ancestor between the current branch and master
	repo := gitHubRepo.GitRepo()
	defaultBranchCommit, err := repo.CommitObject(
		gitHubRepo.DefaultBranchRef().Hash(),
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	headCommit, err := repo.CommitObject(headHash)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	baseCommits, err := headCommit.MergeBase(defaultBranchCommit)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if len(baseCommits) != 1 {
		return nil, errors.New("cannot find a unique merge base")
	}
	baseCommit := baseCommits[0]
	if headCommit.Hash == baseCommit.Hash {
		return nil, errors.New("no new commits")
	}
	deps.DebugLog.Println("found base commit at", baseCommit.Hash)

	// Collect sequence of commits of interest
	commit := headCommit
	var ris []*reviewInfo
	for commit.Hash != baseCommit.Hash {
		ri, err := makeReviewInfo(ctx, gitHubRepo, graphqlClient, commit)
		if err != nil {
			return nil, err
		}
		ris = append(ris, ri)

		status := "review not found"
		if ri.pr != nil {
			status = "reviewID: " + ri.reviewID + " pr: " + ri.pr.GetHTMLURL()
		}
		deps.DebugLog.Println("examined", commit.Hash, status)

		if len(commit.ParentHashes) != 1 {
			return nil, errors.Errorf("commit %v has multiple hashes", commit.Hash)
		}
		commit, err = repo.CommitObject(commit.ParentHashes[0])
		if err != nil {
			return nil, errors.WithStack(err)
		}
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
		}
		ri.headBranch = fmt.Sprintf("plz.review/review/%s", ri.reviewID)
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
	message := ri.commit.Message
	if ri.pr == nil {
		message = strings.TrimRightFunc(ri.commit.Message, unicode.IsSpace) +
			"\n\nplz-review-url: https://plz.review/review/" + ri.reviewID
	}
	newCommit := &object.Commit{
		Author:       ri.commit.Author,
		Committer:    ri.commit.Committer,
		Message:      message,
		TreeHash:     ri.commit.TreeHash,
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
	deps := deps.FromContext(ctx)
	message := ri.commit.Message
	if ri.updatedCommit != nil {
		message = ri.updatedCommit.Message
	}
	parts := strings.SplitN(message, "\n", 2)
	title := strings.TrimSpace(parts[0])
	body := ""
	if len(parts) > 1 {
		body = strings.TrimSpace(parts[1])
	}
	if ri.pr == nil {
		deps.DebugLog.Println("creating PR for head branch", ri.headBranch)
		pr, _, err := gitHubRepo.Client().PullRequests.Create(
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
		err = errors.WithStack(err)
		if len(reviewers) > 0 {
			_, _, err := gitHubRepo.Client().PullRequests.RequestReviewers(
				ctx,
				gitHubRepo.Owner(),
				gitHubRepo.Name(),
				pr.GetNumber(),
				github.ReviewersRequest{
					Reviewers: reviewers,
				},
			)
			err = errors.WithStack(err)
		}
		return true, err
	}
	var reviewersToAdd []string
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
	if ri.pr.Base.GetRef() != ri.baseBranch || ri.pr.GetTitle() != title || ri.pr.GetBody() != body || len(reviewersToAdd) > 0 {
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
		err = errors.WithStack(err)
		if len(reviewersToAdd) > 0 {
			_, _, err := gitHubRepo.Client().PullRequests.RequestReviewers(
				ctx,
				gitHubRepo.Owner(),
				gitHubRepo.Name(),
				ri.pr.GetNumber(),
				github.ReviewersRequest{
					Reviewers: reviewersToAdd,
				},
			)
			err = errors.WithStack(err)
		}
		return true, err
	}
	deps.DebugLog.Println("PR", ri.pr.GetHTMLURL(), "is up to date")
	return false, nil
}

func printReviewInfo(ctx context.Context, ris []*reviewInfo) {
	deps := deps.FromContext(ctx)
	w := tabwriter.NewWriter(deps.InfoLog.Writer(), 0, 0, 1, ' ', 0)
	for i := len(ris) - 1; i >= 0; i-- {
		ri := ris[i]
		parts := strings.SplitN(ri.commit.Message, "\n", 2)
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
		commit := ri.commit
		if ri.updatedCommit != nil {
			commit = ri.updatedCommit
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", commit.Hash.String()[:8], title, reviewURL, status)
	}
	w.Flush()
}

func readReviewIDFromCommitMessage(message string) string {
	s := bufio.NewScanner(strings.NewReader(message))
	for s.Scan() {
		matches := reviewTrailerRegex.FindStringSubmatch(s.Text())
		if len(matches) == 3 {
			return matches[2]
		}
	}
	return ""
}

func makeReviewInfo(
	ctx context.Context,
	gitHubRepo *gitHubRepo,
	graphqlClient *graphql.Client,
	commit *object.Commit,
) (*reviewInfo, error) {
	ri := &reviewInfo{
		commit:   commit,
		reviewID: readReviewIDFromCommitMessage(commit.Message),
	}
	if ri.reviewID == "" {
		return ri, nil
	}
	var query struct {
		Review struct {
			GitHubPR int `graphql:"gitHubPR"`
		} `graphql:"review(id: $id)"`
	}
	err := graphqlClient.Query(ctx, &query, map[string]interface{}{
		"id": graphql.ID(ri.reviewID),
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	pr, _, err := gitHubRepo.Client().PullRequests.Get(
		ctx,
		gitHubRepo.Owner(),
		gitHubRepo.Name(),
		query.Review.GitHubPR,
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
	ri.reviewer = reviewers
	return ri, nil
}
