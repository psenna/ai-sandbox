package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/distribution/reference"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// ErrInvalidImageTag is returned by resolveAgentImageRef (and so by Create)
// for a per-agent image tag that is not a syntactically valid Docker tag.
// internal/api maps it to a 400 on the "image_tag" field.
var ErrInvalidImageTag = errors.New("invalid agent image tag")

// IsInvalidImageTag reports whether err was caused by a malformed per-agent
// image tag.
func IsInvalidImageTag(err error) bool { return errors.Is(err, ErrInvalidImageTag) }

// dateTimeTagRE matches the immutable :YYYYMMDD-HHMMSS tag the agent-image CI
// workflow publishes on every push. Lexical order over this format is
// chronological order, which SortTagsNewestFirst relies on.
var dateTimeTagRE = regexp.MustCompile(`^\d{8}-\d{6}$`)

// imageTagRE is the syntactic rule for a Docker image tag: a leading
// alphanumeric or underscore, then up to 127 more of the same plus '.' and
// '-'. resolveAgentImageRef rejects anything else.
var imageTagRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// IsDateTimeTag reports whether tag is a :YYYYMMDD-HHMMSS date-time tag.
func IsDateTimeTag(tag string) bool { return dateTimeTagRE.MatchString(tag) }

// FilterDateTimeTags keeps only the date-time tags from in, de-duplicated.
// Order is not meaningful in the result -- callers sort it.
func FilterDateTimeTags(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if !IsDateTimeTag(t) {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// SortTagsNewestFirst returns a copy of tags sorted descending lexically,
// which for the :YYYYMMDD-HHMMSS format is newest-first chronologically.
func SortTagsNewestFirst(tags []string) []string {
	out := make([]string, len(tags))
	copy(out, tags)
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// NewestDateTimeTag returns the greatest (newest) date-time tag in tags, or
// "" when there is none.
func NewestDateTimeTag(tags []string) string {
	newest := ""
	for _, t := range tags {
		if IsDateTimeTag(t) && t > newest {
			newest = t
		}
	}
	return newest
}

// ImageTagOf returns the tag of a full image reference, or "" when the
// reference carries no tag (it is untagged or digest-pinned).
func ImageTagOf(ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ""
	}
	if tagged, ok := named.(reference.Tagged); ok {
		return tagged.Tag()
	}
	return ""
}

// RepoWithoutTag returns a full image reference stripped of any tag or
// digest: the registry-qualified "registry/repository" part alone. An
// unparseable reference is returned unchanged.
func RepoWithoutTag(ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ref
	}
	return reference.TrimNamed(named).Name()
}

// resolveAgentImageRef turns a per-agent CreateRequest.ImageTag into the
// concrete image reference the agent should run:
//
//   - "" => the operator's configured AgentImage, verbatim.
//   - a syntactically valid tag => the operator image's repository with that
//     tag substituted in. The tag is NOT checked against the discovered list.
//   - anything else => ErrInvalidImageTag.
func (m *Manager) resolveAgentImageRef(tag string) (string, error) {
	if tag == "" {
		return m.cfg.AgentImage, nil
	}
	if !imageTagRE.MatchString(tag) {
		return "", fmt.Errorf("%w: %q", ErrInvalidImageTag, tag)
	}
	return RepoWithoutTag(m.cfg.AgentImage) + ":" + tag, nil
}

// RefreshAgentImageTags polls the registry for the agent image's tags and
// stores the filtered, sorted result. It never wipes the last-known list: on
// any failure (including no registry client configured) it re-persists the
// previous Tags with an advanced CheckedAt and a LastError, and -- for a real
// poll error -- returns it wrapped.
func (m *Manager) RefreshAgentImageTags(ctx context.Context) error {
	prev, _, prevErr := m.store.GetAgentImageTags(ctx)
	if prevErr != nil {
		// Best-effort: an unreadable snapshot is treated as "no previous
		// list" (the write below then replaces whatever is there), but the
		// read failure itself must not be invisible.
		m.log.WarnContext(ctx, "could not read the last-known agent image tag list", "error", prevErr)
	}
	now := time.Now().UTC()

	if m.registry == nil {
		return m.store.SetAgentImageTags(ctx, store.AgentImageTags{
			Tags:      prev.Tags,
			CheckedAt: now,
			LastError: "registry client not configured",
		})
	}

	raw, err := m.registry.ListTags(ctx)
	if err != nil {
		if serr := m.store.SetAgentImageTags(ctx, store.AgentImageTags{
			Tags:      prev.Tags,
			CheckedAt: now,
			LastError: err.Error(),
		}); serr != nil && ctx.Err() == nil {
			// A write that failed only because ctx is already done is the
			// operator shutting down mid-poll, not something to warn about.
			m.log.WarnContext(ctx, "could not persist the failed agent-image tag refresh", "error", serr)
		}
		return fmt.Errorf("refreshing agent image tags: %w", err)
	}

	return m.store.SetAgentImageTags(ctx, store.AgentImageTags{
		Tags:      SortTagsNewestFirst(FilterDateTimeTags(raw)),
		CheckedAt: now,
	})
}

// AgentImageTags returns the stored agent-image tag snapshot. The bool is
// false when no refresh has ever completed.
func (m *Manager) AgentImageTags(ctx context.Context) (store.AgentImageTags, bool, error) {
	return m.store.GetAgentImageTags(ctx)
}
