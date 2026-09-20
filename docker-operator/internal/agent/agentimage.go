package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/distribution/reference"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/registry"
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

// UpgradeAvailable reports whether a newer agent image exists for an agent
// currently on currentTag. It is true only when currentTag is itself a
// :YYYYMMDD-HHMMSS date-time tag AND at least one entry of tags is a
// date-time tag that sorts strictly after currentTag (lexical order is
// chronological for this format). An agent on :latest, on any non-date-time
// tag, or with an empty currentTag never reports an upgrade. tags is not
// assumed to be pre-filtered or pre-sorted.
func UpgradeAvailable(currentTag string, tags []string) bool {
	if !IsDateTimeTag(currentTag) {
		return false
	}
	for _, t := range tags {
		if IsDateTimeTag(t) && t > currentTag {
			return true
		}
	}
	return false
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

const (
	// defaultImageTagFallback is the tag defaultImageTagFrom lands on when
	// the host holds no tag of the repository and the registry snapshot has
	// no date-time tag either -- the same tag a bare "repo" reference means
	// to Docker, made explicit.
	defaultImageTagFallback = "latest"

	// offeredImageTagLimit is how many of the NEWEST PUBLISHED date-time tags
	// OfferedImageTags offers. Every locally present tag is offered on top of
	// these regardless of age, so a host that is deliberately pinned to an
	// old (or hand-built) image can always pick it back.
	offeredImageTagLimit = 5
)

// ImageTagOption is one selectable agent-image tag: the tag itself, plus
// whether the daemon already holds that image (so the UI can say "no pull
// needed" and a caller can prefer it).
type ImageTagOption struct {
	Tag     string `json:"tag"`
	Present bool   `json:"present"`
}

// ImageInventory is everything the operator knows about one harness's agent
// image in one value: the last-known published snapshot, what the host
// actually holds right now, the tag a new agent gets when nothing pins it,
// and the offer list the two produce. Assembled by AgentImageInventory.
type ImageInventory struct {
	Harness      string           `json:"harness"`
	Repo         string           `json:"repo"`
	RegistryTags []string         `json:"registry_tags"`
	LocalTags    []string         `json:"local_tags"`
	DefaultTag   string           `json:"default_tag"`
	Options      []ImageTagOption `json:"options"`
	CheckedAt    time.Time        `json:"checked_at"`
	LastError    string           `json:"last_error,omitempty"`
}

// registryFor returns the tag-discovery client for harness, or nil when none
// is configured. Indexing a nil map is legal and yields nil, so a Manager
// built with no registries at all is handled here too.
func (m *Manager) registryFor(harness string) registry.Client {
	return m.registries[config.NormalizeHarness(harness)]
}

// nonDateTimeTags keeps only the NON-date-time tags from in, de-duplicated.
func nonDateTimeTags(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if t == "" || IsDateTimeTag(t) {
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

// defaultImageTagFrom picks the tag a new agent runs when nothing pins one,
// preferring what the HOST already has over what the registry has published:
//  1. the newest :YYYYMMDD-HHMMSS tag present on the daemon;
//  2. else the greatest other tag present on the daemon (lexically);
//  3. else the newest published :YYYYMMDD-HHMMSS tag;
//  4. else defaultImageTagFallback (":latest").
//
// Pure: no Manager, no Docker, no registry.
func defaultImageTagFrom(localTags, registryTags []string) string {
	if t := NewestDateTimeTag(localTags); t != "" {
		return t
	}
	if others := nonDateTimeTags(localTags); len(others) > 0 {
		return SortTagsNewestFirst(others)[0]
	}
	if t := NewestDateTimeTag(registryTags); t != "" {
		return t
	}
	return defaultImageTagFallback
}

// OfferedImageTags builds the tag list a chooser should show for one harness:
// the offeredImageTagLimit newest PUBLISHED date-time tags, plus EVERY tag
// the host already holds, plus defaultTag itself. Anything in none of those
// three sets is dropped. Pure: no Manager, no Docker, no registry.
func OfferedImageTags(registryTags, localTags []string, defaultTag string) []ImageTagOption {
	local := make(map[string]struct{}, len(localTags))
	for _, t := range localTags {
		if t == "" {
			continue
		}
		local[t] = struct{}{}
	}

	out := make([]ImageTagOption, 0, offeredImageTagLimit+len(local)+1)
	seen := make(map[string]struct{}, offeredImageTagLimit+len(local)+1)
	add := func(tag string) {
		if tag == "" {
			return
		}
		if _, dup := seen[tag]; dup {
			return
		}
		seen[tag] = struct{}{}
		_, present := local[tag]
		out = append(out, ImageTagOption{Tag: tag, Present: present})
	}

	published := SortTagsNewestFirst(FilterDateTimeTags(registryTags))
	if len(published) > offeredImageTagLimit {
		published = published[:offeredImageTagLimit]
	}
	for _, t := range published {
		add(t)
	}
	for _, t := range SortTagsNewestFirst(FilterDateTimeTags(localTags)) {
		add(t)
	}
	for _, t := range SortTagsNewestFirst(nonDateTimeTags(localTags)) {
		add(t)
	}
	add(defaultTag)

	return out
}

// localImageTags returns the tags of harness's agent image repository that
// the daemon holds RIGHT NOW. Never cached. Best-effort: a daemon error
// degrades to "nothing present" plus a warning, never a failed request.
func (m *Manager) localImageTags(ctx context.Context, harness string) []string {
	h := config.NormalizeHarness(harness)
	repo := RepoWithoutTag(m.AgentImageFor(h))

	imgs, err := m.docker.ImageList(ctx, repo)
	if err != nil {
		m.log.WarnContext(ctx, "could not list the agent images present on the docker daemon; treating the host as holding none",
			"harness", h, "repo", repo, "error", err)
		return nil
	}

	seen := make(map[string]struct{})
	var out []string
	for _, img := range imgs {
		for _, ref := range img.RepoTags {
			if RepoWithoutTag(ref) != repo {
				continue
			}
			tag := ImageTagOf(ref)
			if tag == "" {
				continue
			}
			if _, dup := seen[tag]; dup {
				continue
			}
			seen[tag] = struct{}{}
			out = append(out, tag)
		}
	}
	return out
}

// defaultImageTagFor is defaultImageTagFrom wired to this Manager's live
// local images and stored registry snapshot for harness.
func (m *Manager) defaultImageTagFor(ctx context.Context, harness string) string {
	var published []string
	snap, ok, err := m.AgentImageTags(ctx, harness)
	switch {
	case err != nil:
		m.log.WarnContext(ctx, "could not read the agent image tag snapshot while choosing a default tag",
			"harness", config.NormalizeHarness(harness), "error", err)
	case ok:
		published = snap.Tags
	}
	return defaultImageTagFrom(m.localImageTags(ctx, harness), published)
}

// defaultAgentImageRef is the concrete image reference an agent of this
// harness runs when nothing pins it.
func (m *Manager) defaultAgentImageRef(ctx context.Context, harness string) string {
	return RepoWithoutTag(m.AgentImageFor(harness)) + ":" + m.defaultImageTagFor(ctx, harness)
}

// DefaultAgentImageRef exports defaultAgentImageRef for callers outside this
// package (internal/api's create-form defaults, from a later issue on).
func (m *Manager) DefaultAgentImageRef(ctx context.Context, harness string) string {
	return m.defaultAgentImageRef(ctx, harness)
}

// resolveAgentImageRef turns a per-agent CreateRequest.ImageTag into the
// concrete image reference the agent should run, against the operator's
// default image REPOSITORY for harness:
//   - "" => that repository at defaultImageTagFor's tag.
//   - a syntactically valid tag => that repository with the tag substituted
//     in. The tag is NOT checked against the discovered list.
//   - anything else => ErrInvalidImageTag.
//
// Create passes the request's resolved harness; Update passes the record's
// existing harness (HarnessOf(a)), since Update never changes it.
func (m *Manager) resolveAgentImageRef(ctx context.Context, tag, harness string) (string, error) {
	if tag == "" {
		return m.defaultAgentImageRef(ctx, harness), nil
	}
	if !imageTagRE.MatchString(tag) {
		return "", fmt.Errorf("%w: %q", ErrInvalidImageTag, tag)
	}
	return RepoWithoutTag(m.AgentImageFor(harness)) + ":" + tag, nil
}

// RefreshAgentImageTags polls harness's registry for its agent image's tags
// and stores the filtered, sorted result under that harness's own snapshot
// key. It never wipes the last-known list on failure, and touches ONLY
// harness's registry client and ONLY harness's snapshot key.
func (m *Manager) RefreshAgentImageTags(ctx context.Context, harness string) error {
	h := config.NormalizeHarness(harness)

	prev, _, prevErr := m.store.GetAgentImageTags(ctx, h)
	if prevErr != nil {
		m.log.WarnContext(ctx, "could not read the last-known agent image tag list", "harness", h, "error", prevErr)
	}
	now := time.Now().UTC()

	reg := m.registryFor(h)
	if reg == nil {
		return m.store.SetAgentImageTags(ctx, h, store.AgentImageTags{
			Tags:      prev.Tags,
			CheckedAt: now,
			LastError: "registry client not configured",
		})
	}

	raw, err := reg.ListTags(ctx)
	if err != nil {
		if serr := m.store.SetAgentImageTags(ctx, h, store.AgentImageTags{
			Tags:      prev.Tags,
			CheckedAt: now,
			LastError: err.Error(),
		}); serr != nil && ctx.Err() == nil {
			m.log.WarnContext(ctx, "could not persist the failed agent-image tag refresh", "harness", h, "error", serr)
		}
		return fmt.Errorf("refreshing agent image tags for harness %q: %w", h, err)
	}

	return m.store.SetAgentImageTags(ctx, h, store.AgentImageTags{
		Tags:      SortTagsNewestFirst(FilterDateTimeTags(raw)),
		CheckedAt: now,
	})
}

// AgentImageTags returns harness's stored agent-image tag snapshot.
func (m *Manager) AgentImageTags(ctx context.Context, harness string) (store.AgentImageTags, bool, error) {
	return m.store.GetAgentImageTags(ctx, config.NormalizeHarness(harness))
}

// AgentImageInventory assembles everything a chooser needs for one harness.
func (m *Manager) AgentImageInventory(ctx context.Context, harness string) (ImageInventory, error) {
	h := config.NormalizeHarness(harness)

	snap, _, err := m.AgentImageTags(ctx, h)
	if err != nil {
		return ImageInventory{}, fmt.Errorf("reading the agent image tag snapshot for harness %q: %w", h, err)
	}

	local := m.localImageTags(ctx, h)
	def := defaultImageTagFrom(local, snap.Tags)

	return ImageInventory{
		Harness:      h,
		Repo:         RepoWithoutTag(m.AgentImageFor(h)),
		RegistryTags: snap.Tags,
		LocalTags:    local,
		DefaultTag:   def,
		Options:      OfferedImageTags(snap.Tags, local, def),
		CheckedAt:    snap.CheckedAt,
		LastError:    snap.LastError,
	}, nil
}
