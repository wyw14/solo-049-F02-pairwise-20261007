package pass

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"orbitlink/internal/antenna"
	"orbitlink/internal/audit"
	"orbitlink/internal/satellite"
	"orbitlink/internal/scheduler"
	"orbitlink/internal/store"
)

// Replanner turns every ephemeris revision into a viewable preview. Only
// not-started passes are rescheduled automatically; booked and live contacts
// wait for a manual review decision.
type Replanner struct {
	passes       *Service
	windows      *WindowRecomputer
	queue        *scheduler.CommandQueue
	reservations *scheduler.ReservationPlanner
	audit        *audit.Trail
	mu           sync.RWMutex
	path         string
	previews     []RevisionPreview
}

func NewReplanner(paths store.Paths, passes *Service, windows *WindowRecomputer, queue *scheduler.CommandQueue, reservations *scheduler.ReservationPlanner, trail *audit.Trail) (*Replanner, error) {
	replanner := &Replanner{
		passes: passes, windows: windows, queue: queue, reservations: reservations, audit: trail,
		path: paths.Snapshot("replan-previews"),
	}
	if _, err := store.ReadJSON(replanner.path, &replanner.previews); err != nil {
		return nil, err
	}
	return replanner, nil
}

func (r *Replanner) ApplyRevision(previous, current satellite.Ephemeris) {
	if current.Revision <= previous.Revision {
		return
	}
	shift := time.Duration(current.Revision-previous.Revision) * time.Second
	preview := RevisionPreview{
		ID: uuid.NewString(), SatelliteID: current.SatelliteID,
		PreviousRevision: previous.Revision, CurrentRevision: current.Revision,
		CreatedAt: time.Now().UTC(),
	}
	for _, value := range r.passes.List() {
		if value.SatelliteID != current.SatelliteID {
			continue
		}
		category, impacted := classify(value)
		if !impacted {
			continue
		}
		item := ImpactItem{
			ID: uuid.NewString(), PreviewID: preview.ID, SatelliteID: value.SatelliteID,
			PassID: value.ID, AntennaID: value.AntennaID, Phase: value.Phase, Category: category,
			ReservationID:  value.ReservationID,
			PreviousWindow: value.Window,
			ProposedWindow: Window{AOS: value.Window.AOS.Add(shift), LOS: value.Window.LOS.Add(shift), Revision: current.Revision},
			ShiftSeconds:   int64(shift / time.Second),
		}
		if category == TaskNotStarted {
			item.Disposition = DispositionAuto
			if err := r.applyProposal(item); err != nil {
				item.ReviewNote = err.Error()
				_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.failed", Subject: value.ID, Severity: audit.Warning, Fields: map[string]any{"error": err.Error()}})
			} else {
				item.Applied = true
			}
		} else {
			item.Disposition = DispositionReview
			item.ReviewStatus = ReviewPending
		}
		preview.Items = append(preview.Items, item)
	}
	if len(preview.Items) == 0 {
		return
	}
	r.mu.Lock()
	for index := range r.previews {
		for itemIndex := range r.previews[index].Items {
			existing := &r.previews[index].Items[itemIndex]
			if existing.SatelliteID == current.SatelliteID && existing.ReviewStatus == ReviewPending && existing.ProposedWindow.Revision < current.Revision {
				existing.ReviewStatus = ReviewSuperseded
				existing.ReviewNote = fmt.Sprintf("superseded by revision %d", current.Revision)
			}
		}
	}
	r.previews = append(r.previews, preview)
	err := store.WriteJSON(r.path, r.previews)
	r.mu.Unlock()
	if err != nil {
		_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.preview-lost", Subject: current.SatelliteID, Severity: audit.Warning, Fields: map[string]any{"error": err.Error()}})
		return
	}
	summary := preview.Summary()
	_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.preview", Subject: current.SatelliteID, Fields: map[string]any{
		"preview_id": preview.ID, "previous_revision": previous.Revision, "current_revision": current.Revision,
		"impacts": summary.Total, "auto_rescheduled": summary.AutoRescheduled, "manual_review": summary.PendingReview,
	}})
}

// applyProposal moves one pass to the proposed window and replaces its
// pointing commands. Booked passes also re-reserve the antenna interval.
func (r *Replanner) applyProposal(item ImpactItem) error {
	value, found := r.passes.Get(item.PassID)
	if !found {
		return fmt.Errorf("pass %s not found", item.PassID)
	}
	switch value.Phase {
	case Sealed, Completed, Failed, Canceled:
		return fmt.Errorf("pass %s is %s and cannot be rescheduled", item.PassID, value.Phase)
	}
	window := item.ProposedWindow
	if err := window.Validate(); err != nil {
		return err
	}
	points := []antenna.TrackPoint{
		{At: window.AOS.Add(-20 * time.Second), Azimuth: 120, Elevation: 10},
		{At: window.AOS, Azimuth: 135, Elevation: 20},
		{At: window.LOS, Azimuth: 240, Elevation: 8},
	}
	commands, err := antenna.BuildCommands(value.ID, value.AntennaID, window.Revision, points)
	if err != nil {
		return err
	}
	if err := r.windows.Apply(value.ID, window); err != nil {
		return err
	}
	var interval scheduler.Interval
	if item.Category == TaskBooked {
		interval, err = r.reservations.Refresh(value.ID)
		if err != nil {
			return err
		}
	}
	if err := r.queue.Replace(value.ID, window.Revision, commands); err != nil {
		return err
	}
	return r.passes.update(value.ID, func(pass *Pass) error {
		pass.Window = window
		if item.Category == TaskBooked {
			pass.ReservationID = interval.ReservationID
			pass.ReservationGeneration = interval.Generation
		}
		return nil
	})
}

func (r *Replanner) Previews() []RevisionPreview {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]RevisionPreview(nil), r.previews...)
}

func (r *Replanner) PendingReviews() []ImpactItem {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var items []ImpactItem
	for _, preview := range r.previews {
		for _, item := range preview.Items {
			if item.ReviewStatus == ReviewPending {
				items = append(items, item)
			}
		}
	}
	return items
}

// Approve applies the proposed window of a pending review item as if the pass
// had been rescheduled automatically.
func (r *Replanner) Approve(itemID string) (ImpactItem, error) {
	item, err := r.reviewItem(itemID)
	if err != nil {
		return ImpactItem{}, err
	}
	if item.ReviewStatus != ReviewPending {
		return ImpactItem{}, fmt.Errorf("review item %s is %s", itemID, item.ReviewStatus)
	}
	if err := r.applyProposal(item); err != nil {
		return ImpactItem{}, err
	}
	updated, err := r.mutateItem(itemID, func(current *ImpactItem) error {
		if current.ReviewStatus != ReviewPending {
			return fmt.Errorf("review item %s is %s", itemID, current.ReviewStatus)
		}
		current.Applied = true
		current.ReviewStatus = ReviewApproved
		return nil
	})
	if err != nil {
		return ImpactItem{}, err
	}
	_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.approved", Subject: updated.PassID, Fields: map[string]any{"item_id": updated.ID, "revision": updated.ProposedWindow.Revision, "category": string(updated.Category)}})
	return updated, nil
}

func (r *Replanner) Reject(itemID, reason string) (ImpactItem, error) {
	updated, err := r.mutateItem(itemID, func(current *ImpactItem) error {
		if current.ReviewStatus != ReviewPending {
			return fmt.Errorf("review item %s is %s", itemID, current.ReviewStatus)
		}
		current.ReviewStatus = ReviewRejected
		current.ReviewNote = reason
		return nil
	})
	if err != nil {
		return ImpactItem{}, err
	}
	_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.rejected", Subject: updated.PassID, Fields: map[string]any{"item_id": updated.ID, "revision": updated.ProposedWindow.Revision, "reason": reason}})
	return updated, nil
}

func (r *Replanner) reviewItem(itemID string) (ImpactItem, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, preview := range r.previews {
		for _, item := range preview.Items {
			if item.ID == itemID {
				return item, nil
			}
		}
	}
	return ImpactItem{}, fmt.Errorf("review item %s not found", itemID)
}

func (r *Replanner) mutateItem(itemID string, apply func(*ImpactItem) error) (ImpactItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for previewIndex := range r.previews {
		for itemIndex := range r.previews[previewIndex].Items {
			item := &r.previews[previewIndex].Items[itemIndex]
			if item.ID != itemID {
				continue
			}
			if err := apply(item); err != nil {
				return ImpactItem{}, err
			}
			if err := store.WriteJSON(r.path, r.previews); err != nil {
				return ImpactItem{}, err
			}
			return *item, nil
		}
	}
	return ImpactItem{}, fmt.Errorf("review item %s not found", itemID)
}

func (r *Replanner) QueueInitial(passID string) error {
	value, found := r.passes.Get(passID)
	if !found {
		return fmt.Errorf("pass %s not found", passID)
	}
	points := []antenna.TrackPoint{
		{At: value.Window.AOS.Add(-20 * time.Second), Azimuth: 100, Elevation: 10},
		{At: value.Window.AOS, Azimuth: 125, Elevation: 25},
		{At: value.Window.LOS, Azimuth: 250, Elevation: 7},
	}
	commands, err := antenna.BuildCommands(value.ID, value.AntennaID, value.Window.Revision, points)
	if err != nil {
		return err
	}
	return r.queue.Replace(value.ID, value.Window.Revision, commands)
}
