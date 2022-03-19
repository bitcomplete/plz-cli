package actions

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/AlecAivazis/survey/v2/terminal"
	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pkg/errors"
	"github.com/shurcooL/graphql"
	"github.com/urfave/cli/v2"
)

type review struct {
	ID         string `graphql:"id"`
	Title      string `graphql:"title"`
	Repository struct {
		Name      string `graphql:"name"`
		Namespace struct {
			Name string `graphql:"name"`
		} `graphql:"namespace"`
	} `graphql:"repository"`
	Revisions  []revision `graphql:"revisions"`
	HeadBranch string     `graphql:"headBranch"`
}

type revision struct {
	Number        int    `graphql:"number"`
	HeadCommitSHA string `graphql:"headCommitSha"`
}

type linkedRevisions struct {
	Review   review   `graphql:"review"`
	Revision revision `graphql:"revision"`
}

func (r review) String() string {
	return fmt.Sprintf("(%s) %s", r.ID, r.Title)
}

func Switch(c *cli.Context) error {
	ctx := c.Context
	deps := deps.FromContext(ctx)
	if deps.Auth == nil {
		return errors.New("error loading GitHub credentials, run plz auth")
	}
	gitHubRepo, err := newGitHubRepo(ctx, deps.Auth.Token())
	if err != nil {
		return err
	}
	if err := checkCleanWorktree(ctx, gitHubRepo); err != nil {
		return err
	}
	repo := gitHubRepo.GitRepo()
	headRef, err := repo.Head()
	if err != nil {
		return errors.WithStack(err)
	}
	baseCommit, err := leastCommonAncestor(repo, headRef.Hash(), gitHubRepo.DefaultBranchRef().Hash())
	if err != nil {
		return errors.WithStack(err)
	}
	if headRef.Hash() == baseCommit.Hash {
		return errors.New("not on a branch")
	}

	commit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return errors.WithStack(err)
	}
	reviewID := ""
	for commit.Hash != baseCommit.Hash {
		reviewID = readReviewIDFromCommitMessage(commit.Message)
		if reviewID != "" {
			break
		}
		if len(commit.ParentHashes) != 1 {
			return errors.Errorf("commit %v has multiple parents", commit.Hash)
		}
		var err error
		commit, err = repo.CommitObject(commit.ParentHashes[0])
		if err != nil {
			return errors.WithStack(err)
		}
	}
	if reviewID == "" {
		// TODO: Show the current branch contents instead of erroring.
		return errors.New("could not find stack")
	}

	graphqlClient := graphql.NewClient(deps.PlzAPIBaseURL+"/api/v1", &http.Client{
		Transport: &authTransport{Token: deps.Auth.Token()},
	})
	curReview, err := loadReview(ctx, graphqlClient, reviewID)
	if err != nil {
		return err
	}

	refName := plumbing.NewRemoteReferenceName(
		git.DefaultRemoteName,
		curReview.HeadBranch,
	)
	ref, err := repo.Storer.Reference(refName)
	if err != nil {
		return errors.WithStack(err)
	}
	localHeadCommitSHA := ref.Hash().String()
	var currentRevision *revision
	for i := range curReview.Revisions {
		revision := &curReview.Revisions[i]
		if revision.HeadCommitSHA == localHeadCommitSHA {
			currentRevision = revision
			break
		}
	}
	if currentRevision == nil {
		return errors.New("could not determine current revision")
	}

	linkedRevisions, err := loadLinkedRevisions(ctx, graphqlClient, reviewID, currentRevision.Number)
	if err != nil {
		return err
	}

	refs := map[plumbing.Hash]*plumbing.Reference{headRef.Hash(): headRef}
	reviews := map[string]*review{}
	tipHashes := []plumbing.Hash{headRef.Hash()}
	for i, linkedRevision := range linkedRevisions {
		reviews[linkedRevision.Review.ID] = &linkedRevisions[i].Review

		hash := plumbing.NewHash(linkedRevision.Revision.HeadCommitSHA)
		tipHashes = append(tipHashes, hash)

		refName := plumbing.NewBranchReferenceName(linkedRevision.Review.HeadBranch)
		ref, err := repo.Reference(refName, false)
		// TODO: Handle ErrReferenceNotFound specially
		if err != nil {
			return err
		}
		tipHashes = append(tipHashes, ref.Hash())
		refs[ref.Hash()] = ref

		refName = plumbing.NewRemoteReferenceName(git.DefaultRemoteName, linkedRevision.Review.HeadBranch)
		ref, err = repo.Reference(refName, false)
		// TODO: Handle ErrReferenceNotFound specially
		if err != nil {
			return err
		}
		tipHashes = append(tipHashes, ref.Hash())
	}

	tips, err := bfs(gitHubRepo, tipHashes)
	if err != nil {
		return err
	}
	commits := make(map[plumbing.Hash]*object.Commit, len(tips))
	for _, commit := range tips {
		commits[commit.Hash] = commit

		reviewID := readReviewIDFromCommitMessage(commit.Message)
		if reviewID != "" && reviews[reviewID] == nil {
			review, err := loadReview(ctx, graphqlClient, reviewID)
			if err != nil {
				return err
			}
			reviews[reviewID] = review
		}
	}
	sorted := topologicalSort(tips, commits)

	g := newGraph(commits, os.Stdout)
	items := g.makeSelectItems(sorted, reviews, refs)

	options := make([]string, len(items))
	defaultOption := ""
	for i, item := range items {
		lines := strings.Split(item.text, "\n")
		for i := 1; i < len(lines); i++ {
			lines[i] = "  " + lines[i]
		}
		options[i] = strings.Join(lines, "\n")
		if headRef.Hash().String() == item.hash.String() {
			defaultOption = options[i]
		}
	}
	prompt := &graphSelect{
		Select: survey.Select{
			Options: options,
			Default: defaultOption,
		},
	}
	answer := survey.OptionAnswer{}
	err = survey.AskOne(prompt, &answer)
	if errors.Is(err, terminal.InterruptErr) {
		return nil
	}
	if err != nil {
		return errors.WithStack(err)
	}
	item := items[answer.Index]

	if item.hash.String() == headRef.Hash().String() {
		return nil
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return errors.WithStack(err)
	}
	newHead := ""
	if item.ref != nil {
		newHead = item.ref.Name().Short()
		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: item.ref.Name(),
		})
	} else {
		newHead = item.hash.String()[:7] + " (detached HEAD)"
		err = worktree.Checkout(&git.CheckoutOptions{
			Hash: item.hash,
		})
	}
	if err != nil {
		return errors.WithStack(err)
	}
	fmt.Println(newHead)

	return nil
}

type graphSelect struct {
	survey.Select
}

func (s *graphSelect) Cleanup(*survey.PromptConfig, interface{}) error {
	cursor := s.NewCursor()
	cursor.Restore()
	// Don't render anything on exit, we will render the checkout state
	// separately.
	return s.Render("", struct{}{})
}

func bfs(gitHubRepo *gitHubRepo, tipHashes []plumbing.Hash) ([]*object.Commit, error) {
	repo := gitHubRepo.GitRepo()
	visited := map[plumbing.Hash]struct{}{}
	for _, hash := range tipHashes {
		lca, err := leastCommonAncestor(repo, hash, gitHubRepo.DefaultBranchRef().Hash())
		if err != nil {
			return nil, errors.WithStack(err)
		}
		visited[lca.Hash] = struct{}{}
	}

	var commits []*object.Commit
	queue := make([]plumbing.Hash, len(tipHashes))
	copy(queue, tipHashes)
	for len(queue) > 0 {
		var hash plumbing.Hash
		hash, queue = queue[0], queue[1:]
		if _, ok := visited[hash]; ok {
			continue
		}
		visited[hash] = struct{}{}
		commit, err := repo.CommitObject(hash)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		commits = append(commits, commit)
		queue = append(queue, commit.ParentHashes...)
	}
	return commits, nil
}

func topologicalSort(tips []*object.Commit, commits map[plumbing.Hash]*object.Commit) []*object.Commit {
	inDegree := map[plumbing.Hash]int{}
	for _, commit := range tips {
		inDegree[commit.Hash] = 1
	}
	for _, commit := range tips {
		for _, parentHash := range commit.ParentHashes {
			if commits[parentHash] == nil {
				continue
			}
			if inDegree[parentHash] > 0 {
				inDegree[parentHash] += 1
			}
		}
	}
	var queue []*object.Commit
	for _, commit := range tips {
		if inDegree[commit.Hash] == 1 {
			queue = append(queue, commit)
		}
	}
	sorted := make([]*object.Commit, 0, len(tips))
	for len(queue) > 0 {
		var commit *object.Commit
		commit, queue = queue[0], queue[1:]
		for _, parentHash := range commit.ParentHashes {
			parent := commits[parentHash]
			if parent == nil {
				continue
			}
			if inDegree[parentHash] == 0 {
				continue
			}
			inDegree[parentHash] -= 1
			if inDegree[parentHash] == 1 {
				queue = append(queue, parent)
			}
		}
		inDegree[commit.Hash] = 0
		sorted = append(sorted, commit)
	}
	return sorted
}

func loadReview(ctx context.Context, graphqlClient *graphql.Client, reviewID string) (*review, error) {
	var query struct {
		Review review `graphql:"review(id: $reviewID)"`
	}
	err := graphqlClient.Query(ctx, &query, map[string]interface{}{
		"reviewID": graphql.ID(reviewID),
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &query.Review, nil
}

func loadLinkedRevisions(ctx context.Context, graphqlClient *graphql.Client, reviewID string, revisionNumber int) ([]linkedRevisions, error) {
	var query struct {
		LinkedRevisions []linkedRevisions `graphql:"linkedRevisions(reviewID: $reviewId, revisionNumber: $revisionNumber)"`
	}
	err := graphqlClient.Query(ctx, &query, map[string]interface{}{
		"reviewId":       graphql.ID(reviewID),
		"revisionNumber": graphql.Int(revisionNumber),
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return query.LinkedRevisions, nil
}

func leastCommonAncestor(repo *git.Repository, hash1, hash2 plumbing.Hash) (*object.Commit, error) {
	commit1, err := repo.CommitObject(hash1)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	commit2, err := repo.CommitObject(hash2)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	baseCommits, err := commit1.MergeBase(commit2)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if len(baseCommits) != 1 {
		return nil, errors.New("cannot find a unique merge base")
	}
	return baseCommits[0], nil
}

type graphColumn struct {
	commit *object.Commit
}

type graphState int

const (
	graphStatePadding graphState = iota
	graphStateSkip
	graphStatePreCommit
	graphStateCommit
	graphStatePostMerge
	graphStateCollapsing
)

// graph is an implementation of git graph logging code. It is heavily inspired
// by the asciidag Python implementation:
// https://github.com/sambrightman/asciidag
type graph struct {
	commits map[plumbing.Hash]*object.Commit
	outfile *bufio.Writer

	commit          *object.Commit
	buf             string
	firstParentOnly bool
	numParents      int
	width           int
	expansionRow    int
	state           graphState
	prevState       graphState
	commitIndex     int
	prevCommitIndex int
	numColumns      int
	numNewColumns   int
	mappingSize     int
	columns         map[int]graphColumn
	newColumns      map[int]graphColumn
	mapping         map[int]int
	newMapping      map[int]int
}

func newGraph(commits map[plumbing.Hash]*object.Commit, outfile io.Writer) *graph {
	return &graph{
		commits:    commits,
		outfile:    bufio.NewWriter(outfile),
		columns:    map[int]graphColumn{},
		newColumns: map[int]graphColumn{},
		mapping:    map[int]int{},
		newMapping: map[int]int{},
	}
}

type selectItem struct {
	text string
	hash plumbing.Hash
	ref  *plumbing.Reference
}

func (i selectItem) String() string {
	return i.text
}

func (g *graph) makeSelectItems(sorted []*object.Commit, reviews map[string]*review, refs map[plumbing.Hash]*plumbing.Reference) []selectItem {
	var items []selectItem
	buf := &bytes.Buffer{}
	g.outfile.Reset(buf)

	for _, commit := range sorted {
		buf.Reset()
		g.update(commit)
		g.showCommit()

		var curReview *review
		var curRevision *revision
		reviewID := readReviewIDFromCommitMessage(commit.Message)
		if reviewID != "" {
			curReview = reviews[reviewID]
			if curReview != nil {
				for i, revision := range curReview.Revisions {
					if revision.HeadCommitSHA == commit.Hash.String() {
						curRevision = &curReview.Revisions[i]
						break
					}
				}
			}
		}

		const (
			asciiColorReset  = "\033[m"
			asciiColorYellow = "\033[33m"
			asciiColorGreen  = "\033[32m"
		)
		reviewText := "new review"
		if curReview != nil {
			if curRevision != nil {
				reviewText = fmt.Sprintf("plz.review/review/%s %srevision %d%s", curReview.ID, asciiColorGreen, curRevision.Number, asciiColorReset)
			} else {
				reviewText = fmt.Sprintf("plz.review/review/%s %sunpublished%s", curReview.ID, asciiColorYellow, asciiColorReset)
			}
		}

		parts := strings.SplitN(commit.Message, "\n", 2)
		title := strings.TrimSpace(parts[0])
		if len(title) > 47 {
			title = title[:47] + "..."
		}
		_, _ = g.outfile.WriteString(fmt.Sprintf("%s %s %s", commit.Hash.String()[:8], title, reviewText))
		if !g.isCommitFinished() {
			_, _ = g.outfile.WriteString("\n")
			g.showRemainder()
		}

		g.outfile.Flush()
		items = append(items, selectItem{
			text: buf.String(),
			hash: commit.Hash,
			ref:  refs[commit.Hash],
		})
	}
	return items
}

func (g *graph) writeColumn(col graphColumn, colChar string) {
	g.buf += colChar
}

func (g *graph) updateState(state graphState) {
	g.prevState = g.state
	g.state = state
}

func (g *graph) interestingParents() []*object.Commit {
	hashes := g.commit.ParentHashes
	if g.firstParentOnly && len(hashes) > 1 {
		hashes = hashes[:1]
	}
	parents := make([]*object.Commit, len(hashes))
	for i, hash := range hashes {
		commit, ok := g.commits[hash]
		if !ok {
			continue
		}
		parents[i] = commit
	}
	return parents
}

func (g *graph) insertIntoNewColumns(commit *object.Commit, mappingIndex int) int {
	// If the commit is already in the newColumns list, we don't need to
	// add it. Just update the mapping correctly.
	for i := 0; i < g.numNewColumns; i++ {
		if g.newColumns[i].commit == commit {
			g.mapping[mappingIndex] = i
			return mappingIndex + 2
		}
	}

	// This commit isn't already in newColumns. Add it.
	g.newColumns[g.numNewColumns] = graphColumn{commit: commit}
	g.mapping[mappingIndex] = g.numNewColumns
	g.numNewColumns += 1
	return mappingIndex + 2
}

func (g *graph) updateWidth(isCommitInExistingColumns bool) {
	// Compute the width needed to display the graph for this commit.
	// This is the maximum width needed for any row. All other rows
	// will be padded to this width.
	//
	// Compute the number of columns in the widest row:
	// Count each existing column (g.numColumns), and each new
	// column added by this commit.
	maxCols := g.numColumns + g.numParents

	// Even if the current commit has no parents to be printed, it
	// still takes up a column for itg.
	if g.numParents < 1 {
		maxCols += 1
	}

	// We added a column for the current commit as part of
	// g.numParents. If the current commit was already in
	// g.columns, then we have double counted it.
	if isCommitInExistingColumns {
		maxCols -= 1
	}

	// Each column takes up 2 spaces
	g.width = maxCols * 2
}

func (g *graph) updateColumns() {
	// Swap g.columns with g.newColumns
	// g.columns contains the state for the previous commit,
	// and newColumns now contains the state for our commit.
	//
	// We'll re-use the old columns array as storage to compute the new
	// columns list for the commit after this one.
	g.columns, g.newColumns = g.newColumns, g.columns
	g.numColumns = g.numNewColumns
	g.numNewColumns = 0

	// Now update newColumns and mapping with the information for the
	// commit after this one.
	//
	// First, make sure we have enough room. At most, there will
	// be g.numColumns + g.numParents columns for the next
	// commit.
	maxNewColumns := g.numColumns + g.numParents

	// Clear out g.mapping
	g.mappingSize = 2 * maxNewColumns
	for i := 0; i < g.mappingSize; i++ {
		g.mapping[i] = -1
	}

	// Populate g.newColumns and g.mapping
	//
	// Some of the parents of this commit may already be in
	// g.columns. If so, g.newColumns should only contain a
	// single entry for each such commit. g.mapping should
	// contain information about where each current branch line is
	// supposed to end up after the collapsing is performed.
	seenThis := false
	mappingIdx := 0
	isCommitInColumns := true
	for i := 0; i <= g.numColumns; i++ {
		var colCommit *object.Commit
		if i == g.numColumns {
			if seenThis {
				break
			}
			isCommitInColumns = false
			colCommit = g.commit
		} else {
			colCommit = g.columns[i].commit
		}

		if colCommit == g.commit {
			oldMappingIdx := mappingIdx
			seenThis = true
			g.commitIndex = i
			for _, parent := range g.interestingParents() {
				mappingIdx = g.insertIntoNewColumns(parent, mappingIdx)
			}
			// We always need to increment mappingIdx by at
			// least 2, even if it has no interesting parents.
			// The current commit always takes up at least 2
			// spaces.
			if mappingIdx == oldMappingIdx {
				mappingIdx += 2
			}
		} else {
			mappingIdx = g.insertIntoNewColumns(colCommit, mappingIdx)
		}
	}

	// Shrink mappingSize to be the minimum necessary
	for g.mappingSize > 1 && g.mapping[g.mappingSize-1] < 0 {
		g.mappingSize -= 1
	}

	// Compute g.width for this commit
	g.updateWidth(isCommitInColumns)
}

func (g *graph) update(commit *object.Commit) {
	g.commit = commit
	g.numParents = len(g.interestingParents())

	// Store the old commitIndex in prevCommitIndex.
	// updateColumns() will update g.commitIndex for this
	// commit.
	g.prevCommitIndex = g.commitIndex

	// Call updateColumns() to update
	// columns, newColumns, and mapping.
	g.updateColumns()
	g.expansionRow = 0

	// Update g.state.
	// Note that we don't call updateState() here, since
	// we don't want to update g.prevState. No line for
	// g.state was ever printed.
	//
	// If the previous commit didn't get to the GraphState.PADDING state,
	// it never finished its output. Goto GraphState.SKIP, to print out
	// a line to indicate that portion of the graph is missing.
	//
	// If there are 3 or more parents, we may need to print extra rows
	// before the commit, to expand the branch lines around it and make
	// room for it. We need to do this only if there is a branch row
	// (or more) to the right of this commit.
	//
	// If there are less than 3 parents, we can immediately print the
	// commit line.
	if g.state != graphStatePadding {
		g.state = graphStateSkip
	} else if g.numParents >= 3 && g.commitIndex < g.numColumns-1 {
		g.state = graphStatePreCommit
	} else {
		g.state = graphStateCommit
	}
}

func (g *graph) isMappingCorrect() bool {
	// The mapping is up to date if each entry is at its target,
	// or is 1 greater than its target.
	// (If it is 1 greater than the target, '/' will be printed, so it
	// will look correct on the next row.)
	for i := 0; i < g.mappingSize; i++ {
		target := g.mapping[i]
		if target < 0 {
			continue
		}
		if target == i/2 {
			continue
		}
		return false
	}
	return true
}

func (g *graph) padHorizontally(charsWritten int) {
	// Add additional spaces to the end of the string, so that all
	// lines for a particular commit have the same width.
	//
	// This way, fields printed to the right of the graph will remain
	// aligned for the entire commit.
	if charsWritten >= g.width {
		return
	}

	extra := g.width - charsWritten
	g.buf += strings.Repeat(" ", extra)
}

func (g *graph) outputPaddingLine() {
	// Output a padding row, that leaves all branch lines unchanged
	for i := 0; i < g.numNewColumns; i++ {
		g.writeColumn(g.newColumns[i], "|")
		g.buf += " "
	}

	g.padHorizontally(g.numNewColumns * 2)
}

func (g *graph) outputSkipLine() {
	// Output an ellipsis to indicate that a portion
	// of the graph is missing.
	g.buf += "..."
	g.padHorizontally(3)

	if g.numParents >= 3 && g.commitIndex < g.numColumns-1 {
		g.updateState(graphStatePreCommit)
	} else {
		g.updateState(graphStateCommit)
	}
}

func (g *graph) outputPreCommitLine() {
	// This function formats a row that increases the space around a commit
	// with multiple parents, to make room for it. It should only be
	// called when there are 3 or more parents.
	//
	// We need 2 extra rows for every parent over 2.
	if g.numParents < 3 {
		panic("not enough parents to add expansion row")
	}
	numExpansionRows := (g.numParents - 2) * 2

	// g.expansionRow tracks the current expansion row we are on.
	// It should be in the range [0, numExpansionRows - 1]
	if g.expansionRow < 0 || g.expansionRow >= numExpansionRows {
		panic("wrong number of expansion rows")
	}

	// Output the row
	seenThis := false
	charsWritten := 0
	for i := 0; i < g.numColumns; i++ {
		col := g.columns[i]
		if col.commit == g.commit {
			seenThis = true
			g.writeColumn(col, "|")
			g.buf += strings.Repeat(" ", g.expansionRow)
			charsWritten += 1 + g.expansionRow
		} else if seenThis && g.expansionRow == 0 {
			// This is the first line of the pre-commit output.
			// If the previous commit was a merge commit and
			// ended in the graphStatePostMerge state, all branch
			// lines after g.prevCommitIndex were
			// printed as "\" on the previous line. Continue
			// to print them as "\" on this line. Otherwise,
			// print the branch lines as "|".
			if g.prevState == graphStatePostMerge && g.prevCommitIndex < i {
				g.writeColumn(col, "\\")
			} else {
				g.writeColumn(col, "|")
			}
			charsWritten += 1
		} else if seenThis && g.expansionRow > 0 {
			g.writeColumn(col, "\\")
			charsWritten += 1
		} else {
			g.writeColumn(col, "|")
			charsWritten += 1
		}
		g.buf += " "
		charsWritten += 1
	}

	g.padHorizontally(charsWritten)

	// Increment g.expansionRow,
	// and move to state GraphState.COMMIT if necessary
	g.expansionRow += 1
	if g.expansionRow >= numExpansionRows {
		g.updateState(graphStateCommit)
	}
}

// Draw an octopus merge and return the number of characters written.
func (g *graph) drawOctopusMerge() int {
	// Here dashlessCommits represents the number of parents
	// which don't need to have dashes (because their edges fit
	// neatly under the commit).
	dashlessCommits := 2
	numDashes := (g.numParents-dashlessCommits)*2 - 1
	colNum := 0
	for i := 0; i < numDashes; i++ {
		colNum = i/2 + dashlessCommits + g.commitIndex
		g.writeColumn(g.newColumns[colNum], "-")
	}
	colNum = numDashes/2 + dashlessCommits + g.commitIndex
	g.writeColumn(g.newColumns[colNum], ".")
	return numDashes + 1
}

func (g *graph) outputCommitLine() {
	// Output the row containing this commit
	// Iterate up to and including g.numColumns,
	// since the current commit may not be in any of the existing
	// columns. (This happens when the current commit doesn't have any
	// children that we have already processed.)
	seenThis := false
	charsWritten := 0
	for i := 0; i <= g.numColumns; i++ {
		var col graphColumn
		var colCommit *object.Commit
		if i == g.numColumns {
			if seenThis {
				break
			}
			colCommit = g.commit
		} else {
			col = g.columns[i]
			colCommit = col.commit
		}

		if colCommit == g.commit {
			seenThis = true
			g.buf += "*"
			charsWritten += 1

			if g.numParents > 2 {
				charsWritten += g.drawOctopusMerge()
			}
		} else if seenThis && g.numParents > 2 {
			g.writeColumn(col, "\\")
			charsWritten += 1
		} else if seenThis && g.numParents == 2 {
			// This is a 2-way merge commit.
			// There is no graphStatePreCommit stage for 2-way
			// merges, so this is the first line of output
			// for this commit. Check to see what the previous
			// line of output was.
			//
			// If it was graphStatePostMerge, the branch line
			// coming into this commit may have been '\',
			// and not '|' or '/'. If so, output the branch
			// line as '\' on this line, instead of '|'. This
			// makes the output look nicer.
			if g.prevState == graphStatePostMerge && g.prevCommitIndex < i {
				g.writeColumn(col, "\\")
			} else {
				g.writeColumn(col, "|")
			}
			charsWritten += 1
		} else {
			g.writeColumn(col, "|")
			charsWritten += 1
		}
		g.buf += " "
		charsWritten += 1
	}

	g.padHorizontally(charsWritten)

	if g.numParents > 1 {
		g.updateState(graphStatePostMerge)
	} else if g.isMappingCorrect() {
		g.updateState(graphStatePadding)
	} else {
		g.updateState(graphStateCollapsing)
	}
}

func (g *graph) findNewColumnByCommit(commit *object.Commit) *graphColumn {
	for i := 0; i < g.numNewColumns; i++ {
		if g.newColumns[i].commit.Hash == commit.Hash {
			col := g.newColumns[i]
			return &col
		}
	}
	return nil
}

func (g *graph) outputPostMergeLine() {
	seenThis := false
	charsWritten := 0
	for i := 0; i <= g.numColumns; i++ {
		var col *graphColumn
		var colCommit *object.Commit
		if i == g.numColumns {
			if seenThis {
				break
			}
			colCommit = g.commit
		} else {
			colI := g.columns[i]
			col = &colI
			colCommit = colI.commit
		}

		if colCommit.Hash == g.commit.Hash {
			// Since the current commit is a merge find
			// the columns for the parent commits in
			// newColumns and use those to format the
			// edges.
			seenThis = true
			parents := g.interestingParents()
			if len(parents) == 0 {
				panic("merge has no parents")
			}
			parColumn := g.findNewColumnByCommit(parents[0])
			if parColumn == nil {
				panic("parent column not found")
			}
			g.writeColumn(*parColumn, "|")
			charsWritten += 1
			for _, parent := range parents {
				parColumn = g.findNewColumnByCommit(parent)
				g.writeColumn(*parColumn, "\\")
				g.buf += " "
			}
			charsWritten += (g.numParents - 1) * 2
		} else if seenThis {
			g.writeColumn(*col, "\\")
			g.buf += " "
			charsWritten += 2
		} else {
			g.writeColumn(*col, "|")
			g.buf += " "
			charsWritten += 2
		}
	}

	g.padHorizontally(charsWritten)

	if g.isMappingCorrect() {
		g.updateState(graphStatePadding)
	} else {
		g.updateState(graphStateCollapsing)
	}
}

func (g *graph) outputCollapsingLine() {
	usedHorizontal := false
	horizontalEdge := -1
	horizontalEdgeTarget := -1

	// Clear out the newMapping array
	for i := 0; i < g.mappingSize; i++ {
		g.newMapping[i] = -1
	}

	for i := 0; i < g.mappingSize; i++ {
		target := g.mapping[i]
		if target < 0 {
			continue
		}

		// Since updateColumns() always inserts the leftmost
		// column first, each branch's target location should
		// always be either its current location or to the left of
		// its current location.
		//
		// We never have to move branches to the right. This makes
		// the graph much more legible, since whenever branches
		// cross, only one is moving directions.
		if target*2 > i {
			panic(fmt.Sprintf("position %v targetting column %v", i, target*2))
		}

		if target*2 == i {
			// This column is already in the correct place
			if g.newMapping[i] != -1 {
				panic("new mapping already set")
			}
			g.newMapping[i] = target
		} else if g.newMapping[i-1] < 0 {
			// Nothing is to the left. Move to the left by one.
			g.newMapping[i-1] = target
			// If there isn't already an edge moving horizontally
			// select this one.
			if horizontalEdge == -1 {
				horizontalEdge = i
				horizontalEdgeTarget = target
				// The variable target is the index of the graph
				// column, and therefore target * 2 + 3 is the
				// actual screen column of the first horizontal
				// line.
				for j := target*2 + 3; j < i-2; j += 2 {
					g.newMapping[j] = target
				}
			}
		} else if g.newMapping[i-1] == target {
			// There is a branch line to our left
			// already, and it is our target. We
			// combine with this line, since we share
			// the same parent commit.
			//
			// We don't have to add anything to the
			// output or newMapping, since the
			// existing branch line has already taken
			// care of it.
		} else {
			// There is a branch line to our left,
			// but it isn't our target. We need to
			// cross over it.
			//
			// The space just to the left of this
			// branch should always be empty.
			//
			// The branch to the left of that space
			// should be our eventual target.
			if g.newMapping[i-1] <= target || g.newMapping[i-2] >= 0 || g.newMapping[i-3] != target {
				panic("uh oh")
			}
			g.newMapping[i-2] = target
			// Mark this branch as the horizontal edge to
			// prevent any other edges from moving
			// horizontally.
			if horizontalEdge == -1 {
				horizontalEdge = i
			}
		}
	}

	// The new mapping may be 1 smaller than the old mapping
	if g.newMapping[g.mappingSize-1] < 0 {
		g.mappingSize -= 1
	}

	// Output a line based on the new mapping info
	for i := 0; i < g.mappingSize; i++ {
		target := g.newMapping[i]
		if target < 0 {
			g.buf += " "
		} else if target*2 == i {
			g.writeColumn(g.newColumns[target], "|")
		} else if target == horizontalEdgeTarget && i != horizontalEdge-1 {
			// Set the mappings for all but the
			// first segment to -1 so that they
			// won't continue into the next line.
			if i != target*2+3 {
				g.newMapping[i] = -1
			}
			usedHorizontal = true
			g.writeColumn(g.newColumns[target], "_")
		} else {
			if usedHorizontal && i < horizontalEdge {
				g.newMapping[i] = -1
			}
			g.writeColumn(g.newColumns[target], "/")
		}
	}

	g.padHorizontally(g.mappingSize)
	g.mapping, g.newMapping = g.newMapping, g.mapping

	// If g.mapping indicates that all of the branch lines
	// are already in the correct positions, we are done.
	// Otherwise, we need to collapse some branch lines together.
	if g.isMappingCorrect() {
		g.updateState(graphStatePadding)
	}
}

func (g *graph) nextLine() bool {
	prevState := g.state
	switch g.state {
	case graphStatePadding:
		g.outputPaddingLine()
	case graphStateSkip:
		g.outputSkipLine()
	case graphStatePreCommit:
		g.outputPreCommitLine()
	case graphStateCommit:
		g.outputCommitLine()
	case graphStatePostMerge:
		g.outputPostMergeLine()
	case graphStateCollapsing:
		g.outputCollapsingLine()
	}
	return prevState == graphStateCommit
}

// Output a padding line in the graph.
// This is similar to nextLine(). However, it is guaranteed to
// never print the current commit line. Instead, if the commit line is
// next, it will simply output a line of vertical padding, extending the
// branch lines downwards, but leaving them otherwise unchanged.
func (g *graph) paddingLine() {
	if g.state != graphStateCommit {
		g.nextLine()
		return
	}

	// Output the row containing this commit
	// Iterate up to and including g.numColumns,
	// since the current commit may not be in any of the existing
	// columns. (This happens when the current commit doesn't have any
	// children that we have already processed.)
	for i := 0; i < g.numColumns; i++ {
		col := g.columns[i]
		g.writeColumn(col, "|")
		if col.commit == g.commit && g.numParents > 2 {
			g.buf += strings.Repeat(" ", (g.numParents-2)*2)
		} else {
			g.buf += " "
		}
	}

	g.padHorizontally(g.numColumns)

	// Update g.prevState since we have output a padding line
	g.prevState = graphStatePadding
}

func (g *graph) isCommitFinished() bool {
	return g.state == graphStatePadding
}

func (g *graph) showCommit() {
	shownCommitLine := false

	// When showing a diff of a merge against each of its parents, we
	// are called once for each parent without update having been
	// called. In this case, simply output a single padding line.
	if g.isCommitFinished() {
		g.showPadding()
		shownCommitLine = true
	}

	for !shownCommitLine && !g.isCommitFinished() {
		shownCommitLine = g.nextLine()
		_, _ = g.outfile.WriteString(g.buf)
		if !shownCommitLine {
			_, _ = g.outfile.WriteString("\n")
		}
		g.buf = ""
	}
}

func (g *graph) showPadding() {
	g.paddingLine()
	_, _ = g.outfile.WriteString(g.buf)
	g.buf = ""
}

func (g *graph) showRemainder() bool {
	shown := false

	if g.isCommitFinished() {
		return false
	}

	for {
		g.nextLine()
		_, _ = g.outfile.WriteString(g.buf)
		g.buf = ""
		shown = true

		if g.isCommitFinished() {
			break
		}
		_, _ = g.outfile.WriteString("\n")
	}

	return shown
}
