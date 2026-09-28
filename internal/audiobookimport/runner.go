package audiobookimport

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type DiscoveryReason string

const (
	DiscoveryReasonExplicit  DiscoveryReason = "explicit"
	DiscoveryReasonSeriesGap DiscoveryReason = "series_gap"
	DiscoveryReasonLatest    DiscoveryReason = "latest"
)

type CandidateStatus string

const (
	StatusPlanned  CandidateStatus = "planned"
	StatusImported CandidateStatus = "imported"
	StatusExisting CandidateStatus = "existing"
	StatusSkipped  CandidateStatus = "skipped"
	StatusFailed   CandidateStatus = "failed"
)

const (
	RunReasonOK                  Reason = "ok"
	RunReasonInvalidInput        Reason = "invalid_input"
	RunReasonDuplicateInput      Reason = "duplicate_input"
	RunReasonOwnedVolume         Reason = "owned_volume"
	RunReasonAmbiguousVolume     Reason = "ambiguous_volume"
	RunReasonNonNumericVolume    Reason = "non_numeric_volume"
	RunReasonDuplicateWork       Reason = "duplicate_work"
	RunReasonUnrelatedSeries     Reason = "unrelated_series"
	RunReasonUnrelatedLatest     Reason = "unrelated_latest"
	RunReasonExistingIdentity    Reason = "existing_identity"
	RunReasonCommittedIdentity   Reason = "committed_identity"
	RunReasonAmbiguousIdentity   Reason = "ambiguous_identity"
	RunReasonCandidateCapReached Reason = "candidate_cap_reached"
	RunReasonImportCapReached    Reason = "import_cap_reached"
	RunReasonNoSource            Reason = "no_source"
	RunReasonBelowConfidence     Reason = "below_confidence"
	RunReasonIncoherentRelease   Reason = "incoherent_release"
	RunReasonSourceError         Reason = "source_error"
	RunReasonPublishError        Reason = "publish_error"
	RunReasonProviderError       Reason = "provider_error"
	RunReasonCancelled           Reason = "cancelled"
	RunReasonAlreadyPresent      Reason = "already_present"
)

type DiscoveryProvider interface {
	SearchWorks(ctx context.Context, query string, limit int) ([]AudioSiloWorkCard, error)
	LatestWorks(ctx context.Context, limit int) ([]AudioSiloWorkCard, error)
	WorkDetail(ctx context.Context, id string) (AudioSiloWorkDetail, error)
}

type SeriesGapCandidate struct {
	SeriesID string
	WorkID   string
	Volume   string
	Owned    bool
}

type OwnedInventory interface {
	OwnedAuthorIDs(ctx context.Context) (map[string]bool, error)
	OwnedSeriesIDs(ctx context.Context) (map[string]bool, error)
	SeriesGapCandidates(ctx context.Context, seriesID string) ([]SeriesGapCandidate, error)
}

type CommittedIdentitySource interface {
	ExistingAudiobookshelfIdentities(ctx context.Context) (map[ExternalIdentity]bool, error)
	CommittedIdentities(ctx context.Context) (map[ExternalIdentity]bool, error)
}

type Pacer interface{ Wait(context.Context) error }

type CandidateSourceSelector interface {
	SelectSource(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error)
}

type CandidatePublisher interface {
	Publish(ctx context.Context, req PublicationRequest) (PublicationResult, error)
}

type DiscoveryLimits struct {
	MaxCandidates     int
	MaxImports        int
	MaxLatestExamined int
	MaxSeriesExamined int
	SearchLimit       int
	LatestLimit       int
	Selection         SelectionLimits
}

type RunnerDeps struct {
	Provider   DiscoveryProvider
	Inventory  OwnedInventory
	Identities CommittedIdentitySource
	Selector   CandidateSourceSelector
	Publisher  CandidatePublisher
	Pacer      Pacer
	Limits     DiscoveryLimits
}

type RunRequest struct {
	ExplicitWorkIDs []string
	ExplicitQueries []string
	DryRun          bool
}

type ResultRow struct {
	WorkID  string
	Reasons []DiscoveryReason
	Status  CandidateStatus
	Reason  Reason
}

type RunResult struct{ Rows []ResultRow }

type runCandidate struct {
	workID, series, volume string
	volumeNumber           int
	reasons                []DiscoveryReason
	terminal               *ResultRow
}

func Run(ctx context.Context, deps RunnerDeps, req RunRequest) (RunResult, error) {
	if err := validateRun(ctx, deps, req); err != nil {
		return RunResult{}, err
	}

	existing, err := deps.Identities.ExistingAudiobookshelfIdentities(ctx)
	if err != nil {
		return RunResult{}, safeRunError(ctx, "audiobook existing identity lookup failed")
	}
	committed, err := deps.Identities.CommittedIdentities(ctx)
	if err != nil {
		return RunResult{}, safeRunError(ctx, "audiobook committed identity lookup failed")
	}
	authors, err := deps.Inventory.OwnedAuthorIDs(ctx)
	if err != nil {
		return RunResult{}, safeRunError(ctx, "audiobook author inventory failed")
	}
	series, err := deps.Inventory.OwnedSeriesIDs(ctx)
	if err != nil {
		return RunResult{}, safeRunError(ctx, "audiobook series inventory failed")
	}

	candidates := make([]runCandidate, 0)
	byWork := make(map[string]int)
	add := func(c runCandidate) {
		if i, ok := byWork[c.workID]; ok && c.workID != "" {
			for _, reason := range c.reasons {
				found := false
				for _, have := range candidates[i].reasons {
					if have == reason {
						found = true
						break
					}
				}
				if !found {
					candidates[i].reasons = append(candidates[i].reasons, reason)
				}
			}
			return
		}
		if c.workID != "" {
			byWork[c.workID] = len(candidates)
		}
		candidates = append(candidates, c)
	}

	// Explicit ids and queries always retain caller order and precedence.
	for _, id := range req.ExplicitWorkIDs {
		add(runCandidate{workID: strings.TrimSpace(id), reasons: []DiscoveryReason{DiscoveryReasonExplicit}})
	}
	for _, query := range req.ExplicitQueries {
		hits, searchErr := deps.Provider.SearchWorks(ctx, strings.TrimSpace(query), deps.Limits.SearchLimit)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return RunResult{}, ctxErr
		}
		if searchErr != nil {
			row := ResultRow{WorkID: strings.TrimSpace(query), Reasons: []DiscoveryReason{DiscoveryReasonExplicit}, Status: StatusFailed, Reason: RunReasonProviderError}
			add(runCandidate{workID: row.WorkID, reasons: row.Reasons, terminal: &row})
			continue
		}
		if len(hits) == 0 || strings.TrimSpace(hits[0].ID) == "" {
			row := ResultRow{WorkID: strings.TrimSpace(query), Reasons: []DiscoveryReason{DiscoveryReasonExplicit}, Status: StatusSkipped, Reason: RunReasonInvalidInput}
			add(runCandidate{workID: row.WorkID, reasons: row.Reasons, terminal: &row})
			continue
		}
		add(runCandidate{workID: strings.TrimSpace(hits[0].ID), reasons: []DiscoveryReason{DiscoveryReasonExplicit}})
	}

	// Series branches are canonicalized before merging with latest results.
	seriesIDs := trueKeys(series)
	if len(seriesIDs) > deps.Limits.MaxSeriesExamined {
		seriesIDs = seriesIDs[:deps.Limits.MaxSeriesExamined]
	}
	seriesCandidates := make([]runCandidate, 0)
	discoveryBranches, failedBranches := 1, 0 // latest is an enabled branch
	for _, seriesID := range seriesIDs {
		discoveryBranches++
		gaps, gapErr := deps.Inventory.SeriesGapCandidates(ctx, seriesID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return RunResult{}, ctxErr
		}
		if gapErr != nil {
			failedBranches++
			row := ResultRow{WorkID: seriesID, Reasons: []DiscoveryReason{DiscoveryReasonSeriesGap}, Status: StatusFailed, Reason: RunReasonProviderError}
			seriesCandidates = append(seriesCandidates, runCandidate{workID: seriesID, series: seriesID, reasons: row.Reasons, terminal: &row})
			continue
		}
		seriesCandidates = append(seriesCandidates, classifySeriesGaps(seriesID, gaps)...)
	}
	sort.SliceStable(seriesCandidates, func(i, j int) bool {
		a, b := seriesCandidates[i], seriesCandidates[j]
		if a.series != b.series {
			return a.series < b.series
		}
		if a.volumeNumber != b.volumeNumber {
			return a.volumeNumber < b.volumeNumber
		}
		return a.workID < b.workID
	})
	for _, c := range seriesCandidates {
		add(c)
	}

	latest, latestErr := deps.Provider.LatestWorks(ctx, deps.Limits.LatestLimit)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return RunResult{}, ctxErr
	}
	if latestErr != nil {
		failedBranches++
		row := ResultRow{WorkID: "latest", Reasons: []DiscoveryReason{DiscoveryReasonLatest}, Status: StatusFailed, Reason: RunReasonProviderError}
		add(runCandidate{workID: row.WorkID, reasons: row.Reasons, terminal: &row})
	} else {
		if len(latest) > deps.Limits.MaxLatestExamined {
			latest = latest[:deps.Limits.MaxLatestExamined]
		}
		sort.SliceStable(latest, func(i, j int) bool { return strings.TrimSpace(latest[i].ID) < strings.TrimSpace(latest[j].ID) })
		for _, card := range latest {
			if !latestRelated(card, authors, series) {
				continue
			}
			id := strings.TrimSpace(card.ID)
			if id != "" {
				add(runCandidate{workID: id, reasons: []DiscoveryReason{DiscoveryReasonLatest}})
			}
		}
	}

	result := RunResult{Rows: make([]ResultRow, 0, len(candidates))}
	admitted, imported := 0, 0
	for _, c := range candidates {
		if c.terminal != nil {
			row := *c.terminal
			row.Reasons = append([]DiscoveryReason(nil), c.reasons...)
			result.Rows = append(result.Rows, row)
			continue
		}
		if admitted >= deps.Limits.MaxCandidates {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusSkipped, Reason: RunReasonCandidateCapReached})
			continue
		}
		if admitted > 0 {
			if err := deps.Pacer.Wait(ctx); err != nil {
				return result, contextOrSafe(ctx, err, "audiobook pacing failed")
			}
		}
		admitted++
		detail, detailErr := deps.Provider.WorkDetail(ctx, c.workID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		if detailErr != nil {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusFailed, Reason: RunReasonProviderError})
			continue
		}
		work := workFactsFromDetail(detail)
		if c.series != "" {
			work.Series, work.Volume = c.series, c.volume
		}
		recording, ambiguous := recordingFromDetail(detail)
		if ambiguous {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusSkipped, Reason: RunReasonAmbiguousIdentity})
			continue
		}
		identity, identityErr := ResolveExternalIdentity(work, recording)
		if identityErr != nil {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusSkipped, Reason: RunReasonAmbiguousIdentity})
			continue
		}
		if existing[identity] {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusExisting, Reason: RunReasonExistingIdentity})
			continue
		}
		if committed[identity] {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusExisting, Reason: RunReasonCommittedIdentity})
			continue
		}
		selection, selectErr := deps.Selector.SelectSource(ctx, Target{Work: work, Recording: recording}, deps.Limits.Selection)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		if selectErr != nil {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusFailed, Reason: RunReasonSourceError})
			continue
		}
		if selection.Selected == nil {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusSkipped, Reason: RunReasonNoSource})
			continue
		}
		projected, projectErr := PlanProjection(work, []Decision{*selection.Selected})
		if projectErr != nil {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusSkipped, Reason: RunReasonIncoherentRelease})
			continue
		}
		if !req.DryRun && imported >= deps.Limits.MaxImports {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusSkipped, Reason: RunReasonImportCapReached})
			continue
		}
		published, publishErr := deps.Publisher.Publish(ctx, PublicationRequest{Work: work, Recording: recording, Selection: selection, Projected: projected, DryRun: req.DryRun})
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		if publishErr != nil {
			result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: StatusFailed, Reason: RunReasonPublishError})
			continue
		}
		status, reason := StatusImported, RunReasonOK
		if req.DryRun {
			status = StatusPlanned
		}
		if published.AlreadyPresent {
			status, reason = StatusExisting, RunReasonAlreadyPresent
		}
		if !req.DryRun && status == StatusImported {
			imported++
		}
		result.Rows = append(result.Rows, ResultRow{WorkID: c.workID, Reasons: c.reasons, Status: status, Reason: reason})
	}
	if failedBranches == discoveryBranches && len(req.ExplicitWorkIDs) == 0 && len(req.ExplicitQueries) == 0 {
		return result, errors.New("audiobook discovery sources failed")
	}
	return result, nil
}

const maxDiscoveryLimit = 100000

func validateRun(ctx context.Context, deps RunnerDeps, req RunRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, value := range append(append([]string(nil), req.ExplicitWorkIDs...), req.ExplicitQueries...) {
		if strings.TrimSpace(value) == "" {
			return errors.New("invalid audiobook discovery input")
		}
	}
	for _, dep := range []any{deps.Provider, deps.Inventory, deps.Identities, deps.Selector, deps.Publisher, deps.Pacer} {
		if isNilInterface(dep) {
			return errors.New("missing audiobook discovery dependency")
		}
	}
	l := deps.Limits
	values := []int{l.MaxCandidates, l.MaxImports, l.MaxLatestExamined, l.MaxSeriesExamined, l.SearchLimit, l.LatestLimit,
		l.Selection.MaxQueries, l.Selection.MaxResultsPerQuery, l.Selection.MaxCandidatesInspected}
	for _, n := range values {
		if n <= 0 || n > maxDiscoveryLimit {
			return errors.New("invalid audiobook discovery limits")
		}
	}
	if l.Selection.MaxSourceBytes <= 0 || l.Selection.MaxReleaseSizeBytes <= 0 || l.Selection.MinSeeders < 0 || l.Selection.MinConfidence < 0 {
		return errors.New("invalid audiobook selection limits")
	}
	return nil
}

func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	}
	return false
}

func trueKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key, ok := range values {
		if ok && strings.TrimSpace(key) != "" {
			keys = append(keys, strings.TrimSpace(key))
		}
	}
	sort.Strings(keys)
	return keys
}

func classifySeriesGaps(seriesID string, gaps []SeriesGapCandidate) []runCandidate {
	type parsed struct {
		gap    SeriesGapCandidate
		volume int
		valid  bool
	}
	unique := make([]parsed, 0, len(gaps))
	seenExact := map[string]bool{}
	workVolumes := map[string]map[string]bool{}
	positionWorks := map[int]map[string]bool{}
	for _, gap := range gaps {
		gap.SeriesID, gap.WorkID, gap.Volume = strings.TrimSpace(gap.SeriesID), strings.TrimSpace(gap.WorkID), strings.TrimSpace(gap.Volume)
		key := gap.SeriesID + "\x00" + gap.WorkID + "\x00" + gap.Volume + fmt.Sprint(gap.Owned)
		if seenExact[key] {
			continue
		}
		seenExact[key] = true
		volume, err := strconv.Atoi(gap.Volume)
		valid := err == nil && volume > 0
		unique = append(unique, parsed{gap: gap, volume: volume, valid: valid})
		if gap.WorkID != "" {
			if workVolumes[gap.WorkID] == nil {
				workVolumes[gap.WorkID] = map[string]bool{}
			}
			workVolumes[gap.WorkID][gap.Volume] = true
		}
		if valid {
			if positionWorks[volume] == nil {
				positionWorks[volume] = map[string]bool{}
			}
			positionWorks[volume][gap.WorkID] = true
		}
	}
	result := make([]runCandidate, 0, len(unique))
	emittedWork := map[string]bool{}
	for _, item := range unique {
		gap := item.gap
		if emittedWork[gap.WorkID] {
			continue
		}
		emittedWork[gap.WorkID] = true
		c := runCandidate{workID: gap.WorkID, series: seriesID, volume: gap.Volume, volumeNumber: item.volume, reasons: []DiscoveryReason{DiscoveryReasonSeriesGap}}
		reason := Reason("")
		switch {
		case gap.SeriesID == "" || gap.SeriesID != seriesID || gap.WorkID == "":
			reason = RunReasonUnrelatedSeries
		case gap.Owned:
			reason = RunReasonOwnedVolume
		case !item.valid:
			reason = RunReasonNonNumericVolume
		case len(workVolumes[gap.WorkID]) > 1 || len(positionWorks[item.volume]) > 1:
			reason = RunReasonAmbiguousVolume
		}
		if reason != "" {
			row := ResultRow{WorkID: gap.WorkID, Reasons: c.reasons, Status: StatusSkipped, Reason: reason}
			c.terminal = &row
		}
		result = append(result, c)
	}
	return result
}

func latestRelated(card AudioSiloWorkCard, authors, series map[string]bool) bool {
	for _, author := range card.Authors {
		if authors[strings.TrimSpace(author.ID)] {
			return true
		}
	}
	return card.Series != nil && series[strings.TrimSpace(card.Series.ID)]
}

func workFactsFromDetail(detail AudioSiloWorkDetail) WorkFacts {
	work := WorkFacts{WorkID: strings.TrimSpace(detail.ID), Title: strings.TrimSpace(detail.Title)}
	for _, author := range detail.Authors {
		if name := strings.TrimSpace(author.Name); name != "" {
			work.Authors = append(work.Authors, name)
		}
	}
	if len(detail.Series) > 0 {
		work.Series = strings.TrimSpace(detail.Series[0].Name)
		work.Volume = strings.TrimSpace(detail.Series[0].Position)
	}
	return work
}

func recordingFromDetail(detail AudioSiloWorkDetail) (RecordingFacts, bool) {
	if len(detail.Recordings) > 1 {
		return RecordingFacts{}, true
	}
	if len(detail.Recordings) == 0 {
		return RecordingFacts{}, false
	}
	rec := detail.Recordings[0]
	facts := RecordingFacts{RecordingID: strings.TrimSpace(rec.ID), Language: strings.TrimSpace(detail.Language), Publisher: strings.TrimSpace(rec.Publisher), ChapterCount: rec.ChapterCount, Abridged: rec.Abridged}
	if rec.RuntimeMinutes != nil {
		facts.RuntimeMinutes = *rec.RuntimeMinutes
	}
	for _, narrator := range rec.Narrators {
		if name := strings.TrimSpace(narrator.Name); name != "" {
			facts.Narrators = append(facts.Narrators, name)
		}
	}
	for _, asin := range rec.ASINs {
		if value := strings.TrimSpace(asin.ASIN); value != "" {
			facts.ASINs = append(facts.ASINs, value)
		}
	}
	for _, isbn := range rec.ISBNs {
		if value := strings.TrimSpace(isbn); value != "" {
			facts.ISBNs = append(facts.ISBNs, value)
		}
	}
	return facts, false
}

func safeRunError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New(message)
}

func contextOrSafe(ctx context.Context, err error, message string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New(message)
}
