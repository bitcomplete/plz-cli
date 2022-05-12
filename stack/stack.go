package stack

import (
	"bufio"
	"context"
	"regexp"
	"strings"

	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pkg/errors"
	"github.com/shurcooL/graphql"
)

var (
	reviewTrailerRegex = regexp.MustCompile(
		`^\s*((?i)plz-review-url)\s*:\s+https://plz.review/review/(\w+)\s*$`,
	)
)

type Revision struct {
	ReviewID      string `graphql:"reviewID"`
	Number        int    `graphql:"number"`
	BaseBranch    string `graphql:"baseBranch"`
	BaseCommitSHA string `graphql:"baseCommitSha"`
	HeadCommitSHA string `graphql:"headCommitSha"`
}

type ReviewStatus string

const (
	ReviewStatusDeleted ReviewStatus = "deleted"
	ReviewStatusMerged  ReviewStatus = "merged"
	ReviewStatusOpen    ReviewStatus = "open"
)

type baseReview struct {
	ID         string       `graphql:"id"`
	GitHubPR   int          `graphql:"gitHubPR"`
	HeadBranch string       `graphql:"headBranch"`
	Status     ReviewStatus `graphql:"status"`
	Outdated   bool         `graphql:"outdated"`
}

type Review struct {
	baseReview
	LatestRevision Revision
	LocalRevision  *Revision
}

type CommitStatus string

const (
	CommitStatusBehind   CommitStatus = "behind"
	CommitStatusCurrent  CommitStatus = "current"
	CommitStatusModified CommitStatus = "modified"
	CommitStatusNew      CommitStatus = "new"
)

type CommitInfo struct {
	Commit *object.Commit
	*Review
}

func (ci *CommitInfo) Status() CommitStatus {
	review := ci.Review
	if review == nil {
		return CommitStatusNew
	}
	if review.LocalRevision == nil {
		return CommitStatusModified
	}
	if review.LocalRevision.Number != review.LatestRevision.Number {
		return CommitStatusBehind
	}
	return CommitStatusCurrent
}

type CommitStack []CommitInfo

// Load returns the review stack starting at the given head commit.
func Load(
	ctx context.Context,
	repo *git.Repository,
	graphqlClient *graphql.Client,
	headCommit *object.Commit,
	defaultBranch string,
) (CommitStack, error) {
	deps := deps.FromContext(ctx)

	// Find the merge base of the head commit and the default branch.
	defaultBranchRefName := plumbing.NewRemoteReferenceName(
		git.DefaultRemoteName,
		defaultBranch,
	)
	defaultBranchRef, err := repo.Reference(defaultBranchRefName, true)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defaultBranchCommit, err := repo.CommitObject(defaultBranchRef.Hash())
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
	deps.DebugLog.Printf("merge base commit is %v", baseCommit.Hash)

	// Walk up the commit history until we find a commit matching a revision or
	// we hit the default branch. Everything up to that point will consist of
	// new or modified reviews. Everything after that will consist existing
	// review revisions that we will fetch via linked revisions.
	s := CommitStack{}
	visitedReviews := map[string]struct{}{}
	var localRevisionParent, latestRevisionParent *Revision
	for commit := headCommit; commit.Hash != baseCommit.Hash; {
		ci := CommitInfo{Commit: commit}
		deps.DebugLog.Printf("processing commit %v", commit.Hash)
		deps.DebugLog.Printf("commit %v has parents %v", commit.Hash, commit.ParentHashes)
		nextCommit, err := repo.CommitObject(commit.ParentHashes[0])
		if err != nil {
			return nil, errors.WithStack(err)
		}
		reviewID := readReviewIDFromCommitMessage(commit.Message)
		if reviewID == "" {
			// This is a new review.
			s = append(s, ci)
			commit = nextCommit
			continue
		}
		var query struct {
			Review struct {
				baseReview
				LatestRevisionList struct {
					Revisions []struct {
						Revision
						Parent *Revision `graphql:"parent"`
					} `graphql:"revisions"`
				} `graphql:"latestRevisionList: revisionList(options: {count: 1})"`
				LocalRevisionList struct {
					Revisions []struct {
						Revision
						Parent *Revision `graphql:"parent"`
					} `graphql:"revisions"`
				} `graphql:"localRevisionList: revisionList(options: {count: 1}, filterOptions: {headCommitSha: $headCommitSha})"`
			} `graphql:"review(id: $reviewId)"`
		}
		deps.DebugLog.Printf("loading review %v", reviewID)
		err = graphqlClient.Query(ctx, &query, map[string]interface{}{
			"reviewId":      graphql.ID(reviewID),
			"headCommitSha": graphql.String(commit.Hash.String()),
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}
		review := query.Review
		latestRevision := review.LatestRevisionList.Revisions[0]
		latestRevisionParent = latestRevision.Parent
		var localRevision *Revision
		if localRevisions := review.LocalRevisionList.Revisions; len(localRevisions) > 0 {
			localRevision = &localRevisions[0].Revision
			localRevisionParent = localRevisions[0].Parent
		}
		ci.Review = &Review{
			baseReview:     review.baseReview,
			LatestRevision: latestRevision.Revision,
			LocalRevision:  localRevision,
		}
		s = append(s, ci)
		visitedReviews[reviewID] = struct{}{}
		if localRevision != nil {
			// The current commit matches a revision that exists in this review.
			// Subsequent commits will come from linked revisions.
			break
		}
		commit = nextCommit
	}

	// Determine the starting point for loading linked revisions, defaulting to
	// the parent of the local revision found above.
	startRevision := localRevisionParent
	if startRevision == nil && latestRevisionParent != nil {
		// No local revision was found before we reached the default branch.
		// Subsequent reviews will come from linked revisions starting from the
		// last review's parent. Use as a starting point the latest revision of
		// that parent. Importantly, we can't just use latestRevisionParent
		// because it might be behind, e.g. if the parent was merged and the
		// local repo hasn't been synced.
		var query struct {
			Review struct {
				LatestRevisionList struct {
					Revisions []Revision `graphql:"revisions"`
				} `graphql:"latestRevisionList: revisionList(options: {count: 1})"`
			} `graphql:"review(id: $reviewId)"`
		}
		err := graphqlClient.Query(ctx, &query, map[string]interface{}{
			"reviewId": graphql.ID(latestRevisionParent.ReviewID),
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}
		startRevision = &query.Review.LatestRevisionList.Revisions[0]
	}

	if startRevision != nil {
		// We have a starting point so load the linked revisions.
		var query struct {
			LinkedRevisions []struct {
				Review struct {
					baseReview
					LatestRevisionList struct {
						Revisions []Revision `graphql:"revisions"`
					} `graphql:"latestRevisionList: revisionList(options: {count: 1})"`
				} `graphql:"review"`
				Revision `graphql:"revision"`
			} `graphql:"linkedRevisions(reviewID: $reviewId, revisionNumber: $revisionNumber, direction: ancestors)"`
		}
		deps.DebugLog.Printf(
			"loading linkedRevisions for review %v revision %v",
			startRevision.ReviewID,
			startRevision.Number,
		)
		err := graphqlClient.Query(ctx, &query, map[string]interface{}{
			"reviewId":       graphql.ID(startRevision.ReviewID),
			"revisionNumber": graphql.Int(startRevision.Number),
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}
		linkedRevisions := query.LinkedRevisions
		for i := len(linkedRevisions) - 1; i >= 0; i-- {
			linkedRevision := linkedRevisions[i]
			if _, ok := visitedReviews[linkedRevision.Review.ID]; ok {
				// This review has been visited before which most likely means
				// that the stack has been reordered.
				//
				// TODO: One situation where this is OK is if a review above the
				// default branch was a child of this review, but then the
				// commits were reordered. In this case we should just skip this
				// review since it will be sorted out when plz review is run.
				// However, there are certainly other, more problematic cases,
				// so this needs more careful thought.
				continue
			}
			headCommitHash := plumbing.NewHash(linkedRevision.Revision.HeadCommitSHA)
			commit, err := repo.CommitObject(headCommitHash)
			if err != nil {
				if errors.Is(err, plumbing.ErrObjectNotFound) {
					// If we haven't yet fetched the commit from the remote then
					// we fabricate a commit and hope downstream code doesn't
					// try to access anything other than the hash.
					commit = &object.Commit{Hash: headCommitHash}
				} else {
					return nil, errors.WithStack(err)
				}
			}
			review := &linkedRevision.Review
			s = append(s, CommitInfo{
				Commit: commit,
				Review: &Review{
					baseReview:     review.baseReview,
					LatestRevision: review.LatestRevisionList.Revisions[0],
					LocalRevision:  &linkedRevision.Revision,
				},
			})
		}
	}
	return s, nil
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
