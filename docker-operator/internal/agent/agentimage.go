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
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
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

// ErrImageTagInUse is returned by DeleteAgentImageTag (and reported per tag
// by CleanupAgentImages) for a tag some agent record still references --
// either by its Image reference or by the image ID the record's ImageID
// names. internal/api maps it to a 409.
var ErrImageTagInUse = errors.New("agent image tag is in use")

// IsImageTagInUse reports whether err was caused by an in-use image tag.
func IsImageTagInUse(err error) bool { return errors.Is(err, ErrImageTagInUse) }

// ErrImageTagMissing is returned by DeleteAgentImageTag for a tag of the
// harness's agent image repository the daemon does not hold. internal/api maps
// it to a 404.
var ErrImageTagMissing = errors.New("agent image tag is not present on the daemon")

// IsImageTagMissing reports whether err was caused by an absent image tag.
func IsImageTagMissing(err error) bool { return errors.Is(err, ErrImageTagMissing) }

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
//
// The RESULT is one flat list ordered by tag name, newest first (descending
// lexical order, which is chronological for :YYYYMMDD-HHMMSS). It is
// deliberately NOT grouped by provenance: which set a tag came from decides
// only whether it is offered, never where it sorts. An earlier version did
// group them -- published, then host-held -- which meant a tag the host
// already had, and that was NEWER than a published one, still appeared below
// it; on a repository whose tags are all dates the chooser then read as
// almost-sorted, which is worse than either extreme.
func OfferedImageTags(registryTags, localTags []string, defaultTag string) []ImageTagOption {
	local := make(map[string]struct{}, len(localTags))
	for _, t := range localTags {
		if t == "" {
			continue
		}
		local[t] = struct{}{}
	}

	offered := make(map[string]struct{}, offeredImageTagLimit+len(local)+1)
	add := func(tag string) {
		if tag != "" {
			offered[tag] = struct{}{}
		}
	}

	published := SortTagsNewestFirst(FilterDateTimeTags(registryTags))
	if len(published) > offeredImageTagLimit {
		published = published[:offeredImageTagLimit]
	}
	for _, t := range published {
		add(t)
	}
	for _, t := range localTags {
		add(t)
	}
	add(defaultTag)

	tags := make([]string, 0, len(offered))
	for t := range offered {
		tags = append(tags, t)
	}

	out := make([]ImageTagOption, 0, len(tags))
	for _, t := range SortTagsNewestFirst(tags) {
		_, present := local[t]
		out = append(out, ImageTagOption{Tag: t, Present: present})
	}
	return out
}

// localImageTags returns the tags of harness's agent image repository that
// the daemon holds RIGHT NOW. Never cached. Best-effort: a daemon error
// degrades to "nothing present" plus a warning, never a failed request.
//
// repo is handed to ImageList in the NORMALIZED form (RepoWithoutTag parses
// through distribution/reference, so "myorg/agent-image" becomes
// "docker.io/myorg/agent-image"); dockerclient canonicalizes it back to the
// familiar form the daemon speaks internally (#205), so no caller here has to
// care which form the daemon wants.
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

// agentImageUsage is what one harness's agent RECORDS pin: the tags they were
// created against and the image IDs their containers last started from. It is
// the "in use" half of every removal decision below.
type agentImageUsage struct {
	// tags are the tags some record's Image (repo:tag) names.
	tags map[string]struct{}
	// ids are the image IDs some record's ImageID matches.
	ids map[string]struct{}
}

// agentImageUsage reads every agent record -- any status: a stopped or errored
// agent's image is still its image, and a record mid-create or mid-delete can
// be holding a slot and an image -- and buckets, per harness, the tags and
// image IDs they reference. A record's Image counts only when it names THIS
// harness's repository (a stale record from a different repo protects
// nothing here); ImageID counts regardless of repo, because a content digest
// is not tied to the repository it was pulled through.
func (m *Manager) agentImageUsage(ctx context.Context) (map[string]agentImageUsage, error) {
	agents, err := m.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the agent records to decide which image tags are in use: %w", err)
	}
	out := make(map[string]agentImageUsage, len(config.Harnesses()))
	for _, h := range config.Harnesses() {
		out[h] = agentImageUsage{tags: map[string]struct{}{}, ids: map[string]struct{}{}}
	}
	for _, a := range agents {
		h := HarnessOf(a)
		u, ok := out[h]
		if !ok {
			u = agentImageUsage{tags: map[string]struct{}{}, ids: map[string]struct{}{}}
			out[h] = u
		}
		// RepoWithoutTag normalizes both sides, so a hand-configured bare repo
		// and the operator's fully-qualified default compare consistently.
		if a.Image != "" && RepoWithoutTag(a.Image) == RepoWithoutTag(m.AgentImageFor(h)) {
			if tag := ImageTagOf(a.Image); tag != "" {
				u.tags[tag] = struct{}{}
			}
		}
		if a.ImageID != "" {
			u.ids[a.ImageID] = struct{}{}
		}
	}
	return out, nil
}

// imageTagID resolves the image ID a repo:tag ref currently points at, from
// an ImageList result (one entry per image, each carrying every tag it has).
// "" when the tag is not among the entries.
func imageTagID(images []dockerclient.Image, tag string) string {
	for _, img := range images {
		for _, rt := range img.RepoTags {
			if ImageTagOf(rt) == tag {
				return img.ID
			}
		}
	}
	return ""
}

// DeleteAgentImageTag removes ONE tag of harness's agent image repository from
// the daemon -- the manual scalpel. It refuses, without removing anything:
//
//   - ErrInvalidImageTag  -- tag is not a syntactically valid Docker tag;
//   - ErrImageTagMissing  -- the daemon holds no such tag of that repository;
//   - ErrImageTagInUse    -- some agent record's Image names the tag, or the
//     tag points at an image ID some record's ImageID matches.
//
// Everything else is deletable ON PURPOSE, including the literal ":latest" and
// the harness's newest date-time tag: this is the user overriding; the bulk
// CleanupAgentImages below is the action that carries the extra protections.
func (m *Manager) DeleteAgentImageTag(ctx context.Context, harness, tag string) error {
	h := config.NormalizeHarness(harness)
	if !imageTagRE.MatchString(tag) {
		return fmt.Errorf("%w: %q", ErrInvalidImageTag, tag)
	}
	repo := RepoWithoutTag(m.AgentImageFor(h))

	// Both guards before the remove, and a failure to read either is a
	// failure to remove: a cleanup that cannot see what is in use must not
	// delete anything.
	usage, err := m.agentImageUsage(ctx)
	if err != nil {
		return err
	}
	images, err := m.docker.ImageList(ctx, repo)
	if err != nil {
		return fmt.Errorf("listing the images of %q: %w", repo, err)
	}
	u := usage[h]

	var id string
	present := false
	for _, img := range images {
		for _, rt := range img.RepoTags {
			if ImageTagOf(rt) == tag {
				present, id = true, img.ID
			}
		}
	}
	if !present {
		return fmt.Errorf("%w: %q of %q", ErrImageTagMissing, tag, repo)
	}
	if _, named := u.tags[tag]; named {
		return fmt.Errorf("%w: an agent record was created against %q", ErrImageTagInUse, repo+":"+tag)
	}
	if id != "" {
		if _, pinned := u.ids[id]; pinned {
			return fmt.Errorf("%w: the image %q points at is one an agent record runs", ErrImageTagInUse, repo+":"+tag)
		}
	}

	return m.docker.ImageRemove(ctx, repo+":"+tag)
}

// AgentImageCleanupReport is what CleanupAgentImages did for ONE harness.
//
// Removed lists the tags it removed -- an entry counts when the TAG is gone,
// even if the image survives through another tag (the daemon "untags"), so the
// count is tags, not reclaimed bytes. Failed carries per-tag daemon refusals
// (an image a leaked container still references, say); the rest were still
// attempted. Error is set only when NOTHING was attempted: the store read or
// the daemon image list failed, and a cleanup that cannot see what is in use
// must not delete anything.
type AgentImageCleanupReport struct {
	Harness string            `json:"harness"`
	Removed []string          `json:"removed"`
	Failed  map[string]string `json:"failed,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// CleanupAgentImages removes every tag of harness's agent image repository
// that (1) no agent record references -- neither by Image (see
// agentImageUsage) nor by the image ID its ImageID names -- and (2) is not the
// harness's newest LOCAL date-time tag. That second protection is what keeps
// the offline fallback defaultImageTagFrom hands new agents; the newest LOCAL
// tag is kept even when the registry has published a newer one, because the
// local one is what serves a create when the registry is unreachable. The
// literal ":latest" and any other non-date-time tag get no blanket protection
// -- only usage decides for them.
//
// A per-tag removal failure does not stop the rest; it is recorded in
// Failed and the call still reports success.
func (m *Manager) CleanupAgentImages(ctx context.Context, harness string) (AgentImageCleanupReport, error) {
	h := config.NormalizeHarness(harness)
	repo := RepoWithoutTag(m.AgentImageFor(h))
	report := AgentImageCleanupReport{Harness: h}

	usage, err := m.agentImageUsage(ctx)
	if err != nil {
		return report, err
	}
	images, err := m.docker.ImageList(ctx, repo)
	if err != nil {
		return report, fmt.Errorf("listing the images of %q: %w", repo, err)
	}
	u := usage[h]

	// The local tags ARE the ImageList result's RepoTags; collect them once,
	// sorted so the walk (and so the report, and so a test's "the rest were
	// still attempted" assertion) is deterministic regardless of the daemon's
	// or the fake's listing order.
	localTags := make([]string, 0, len(images))
	for _, img := range images {
		for _, rt := range img.RepoTags {
			localTags = append(localTags, ImageTagOf(rt))
		}
	}
	sort.Strings(localTags)
	newest := NewestDateTimeTag(localTags)

	for _, tag := range localTags {
		if _, named := u.tags[tag]; named {
			continue
		}
		// imageTagID finds the ID even though it appears in a multi-tag
		// entry, so an aliased tag is protected through whichever tag the
		// record named.
		if id := imageTagID(images, tag); id != "" {
			if _, pinned := u.ids[id]; pinned {
				continue
			}
		}
		if tag == newest {
			continue
		}
		if err := m.docker.ImageRemove(ctx, repo+":"+tag); err != nil {
			if report.Failed == nil {
				report.Failed = map[string]string{}
			}
			report.Failed[tag] = err.Error()
			continue
		}
		report.Removed = append(report.Removed, tag)
	}
	return report, nil
}

// AgentImagePullReport is the outcome of PullLatestAgentImage for ONE
// harness: Tag is the tag that was pulled, empty when Error is set.
type AgentImagePullReport struct {
	Harness string `json:"harness"`
	Tag     string `json:"tag,omitempty"`
	Error   string `json:"error,omitempty"`
}

// agentImagePullTimeout bounds one harness's blocking pull. A large agent
// image over a slow link takes minutes; the API server sets no WriteTimeout
// (only ReadHeaderTimeout), so this is the only bound on the request the
// handler holds open.
const agentImagePullTimeout = 10 * time.Minute

// PullLatestAgentImage pulls the newest PUBLISHED :YYYYMMDD-HHMMSS tag of
// harness's agent image and blocks until the pull has finished.
//
// The stored registry snapshot is refreshed FIRST, best-effort
// (RefreshAgentImageTags's own policy keeps the last-known list on a failure):
// pulling from a stale snapshot would install yesterday's image while the panel
// advertises today's. The pull is unconditional -- no inspect-first -- because
// date-time tags are immutable per the CI that publishes them, so an
// already-present tag means the daemon no-ops the pull, and "pull latest" must
// mean a real registry round-trip regardless.
//
// Outcomes the UI must word -- no registry client configured, nothing
// published yet -- are Error fields on the report, not returned errors.
func (m *Manager) PullLatestAgentImage(ctx context.Context, harness string) AgentImagePullReport {
	h := config.NormalizeHarness(harness)
	report := AgentImagePullReport{Harness: h}

	if m.registryFor(h) == nil {
		report.Error = "no registry client is configured for this harness's agent image repository"
		return report
	}
	if err := m.RefreshAgentImageTags(ctx, h); err != nil {
		// Keep going on the stale snapshot rather than refusing: it is the
		// newest thing we know, and it may be exactly what the user wants
		// offline.
		m.log.WarnContext(ctx, "refreshing the agent image tag snapshot before a pull failed; pulling from the last-known list",
			"harness", h, "error", err)
	}
	snap, _, err := m.AgentImageTags(ctx, h)
	if err != nil {
		report.Error = "reading the agent image tag snapshot failed: " + err.Error()
		return report
	}
	tag := NewestDateTimeTag(snap.Tags)
	if tag == "" {
		report.Error = "no published date-time tag is known for this harness yet -- press Check now"
		return report
	}

	pullCtx, cancel := context.WithTimeout(ctx, agentImagePullTimeout)
	defer cancel()
	ref := RepoWithoutTag(m.AgentImageFor(h)) + ":" + tag
	if err := m.docker.ImagePull(pullCtx, ref); err != nil {
		report.Error = "pulling " + ref + " failed: " + err.Error()
		return report
	}
	report.Tag = tag
	return report
}
