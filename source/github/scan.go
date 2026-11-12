package github

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"slices"

	"github.com/umran/grafsol/source"
)

// scanPosition is what a scan checkpoint holds: where to carry on from.
type scanPosition struct {
	// Repository and Page name the next page of issues to fetch.
	Repository string `json:"repository,omitempty"`
	Page       int    `json:"page,omitempty"`

	// Done is set once the last page of the last repository has been read.
	Done bool `json:"done,omitempty"`
}

// Scan implements source.Scanner. It lists every issue and pull request of
// every configured repository, one page at a time.
//
// The last change of each page carries a checkpoint that names the page
// after it, so an interrupted scan resumes at the page it had reached. If
// GitHub's rate limit runs out part-way, the scan ends with a
// *RateLimitError and can be resumed once the limit has reset.
func (s *Source) Scan(ctx context.Context, resume source.Checkpoint) iter.Seq2[source.Change, error] {
	return func(yield func(source.Change, error) bool) {
		start, err := s.scanStart(resume)
		if err != nil {
			yield(source.Change{}, err)
			return
		}
		if start.Done {
			return
		}

		first := slices.Index(s.repositories, start.Repository)
		for at := first; at < len(s.repositories); at++ {
			repository := s.repositories[at]
			page := 1
			if at == first {
				page = start.Page
			}

			for ; ; page++ {
				items, err := s.listIssues(ctx, repository, page)
				if err != nil {
					yield(source.Change{}, err)
					return
				}
				// A page that is not full is the repository's last.
				lastPage := len(items) < perPage

				// Work out where a scan should carry on once this page
				// has been dealt with.
				next := scanPosition{Repository: repository, Page: page + 1}
				if lastPage {
					if at+1 < len(s.repositories) {
						next = scanPosition{Repository: s.repositories[at+1], Page: 1}
					} else {
						next = scanPosition{Done: true}
					}
				}

				for i, item := range items {
					change, err := s.change(repository, item, false)
					if err != nil {
						yield(source.Change{}, err)
						return
					}
					if i == len(items)-1 {
						if change.Checkpoint, err = json.Marshal(next); err != nil {
							yield(source.Change{}, err)
							return
						}
					}
					if !yield(change, nil) {
						return
					}
				}
				if lastPage {
					break
				}
			}
		}
	}
}

// scanStart works out where a scan begins: at the first page of the first
// repository, or wherever the checkpoint says.
func (s *Source) scanStart(resume source.Checkpoint) (scanPosition, error) {
	beginning := scanPosition{Repository: s.repositories[0], Page: 1}
	if len(resume) == 0 {
		return beginning, nil
	}
	var position scanPosition
	if err := json.Unmarshal(resume, &position); err != nil {
		return scanPosition{}, fmt.Errorf("github: the scan checkpoint is unreadable: %w", err)
	}
	if position.Done {
		return position, nil
	}
	if !s.covered[position.Repository] || position.Page < 1 {
		// The checkpoint is from a time when other repositories were
		// configured. Starting over is always safe: it only repeats work.
		return beginning, nil
	}
	return position, nil
}
