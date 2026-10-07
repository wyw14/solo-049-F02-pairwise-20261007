package pass

import (
	"fmt"
	"time"

	"orbitlink/internal/antenna"
	"orbitlink/internal/audit"
	"orbitlink/internal/satellite"
	"orbitlink/internal/scheduler"
)

type Replanner struct {
	passes       *Service
	windows      *WindowRecomputer
	queue        *scheduler.CommandQueue
	timeline     *scheduler.Timeline
	reservations *scheduler.ReservationPlanner
	previews     *PreviewStore
	audit        *audit.Trail
}

func NewReplanner(passes *Service, windows *WindowRecomputer, queue *scheduler.CommandQueue, timeline *scheduler.Timeline, reservations *scheduler.ReservationPlanner, previews *PreviewStore, trail *audit.Trail) *Replanner {
	return &Replanner{passes: passes, windows: windows, queue: queue, timeline: timeline, reservations: reservations, previews: previews, audit: trail}
}

// ApplyRevision recomputes the plan for a new ephemeris revision and records a
// preview of every impact. Only passes that have not started are rescheduled
// automatically; booked and tracking passes wait for operator review.
func (r *Replanner) ApplyRevision(previous, current satellite.Ephemeris) {
	if current.Revision <= previous.Revision {
		return
	}
	preview := Preview{SatelliteID: current.SatelliteID, FromRevision: previous.Revision, ToRevision: current.Revision}
	for _, value := range r.passes.List() {
		if value.SatelliteID != current.SatelliteID {
			continue
		}
		class, affected := r.classify(value)
		if !affected {
			continue
		}
		shift := time.Duration(current.Revision-previous.Revision) * time.Second
		window := Window{AOS: value.Window.AOS.Add(shift), LOS: value.Window.LOS.Add(shift), Revision: current.Revision}
		impact := Impact{
			PassID: value.ID, SatelliteID: value.SatelliteID, Class: class,
			PreviousWindow: value.Window, ProposedWindow: window,
		}
		if class == ClassNotStarted {
			if err := r.applyWindow(value, window); err != nil {
				_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.failed", Subject: value.ID, Severity: audit.Warning, Fields: map[string]any{"error": err.Error()}})
				continue
			}
			impact.Disposition = AutoApplied
		} else {
			impact.Disposition = PendingReview
			_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.review_pending", Subject: value.ID, Severity: audit.Warning, Fields: map[string]any{"class": string(class), "revision": current.Revision, "proposed_aos": window.AOS, "proposed_los": window.LOS}})
		}
		preview.Impacts = append(preview.Impacts, impact)
	}
	if len(preview.Impacts) == 0 {
		return
	}
	if err := r.previews.SupersedePending(current.SatelliteID); err != nil {
		_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.supersede_failed", Subject: current.SatelliteID, Severity: audit.Warning, Fields: map[string]any{"error": err.Error()}})
	}
	saved, err := r.previews.Save(preview)
	if err != nil {
		_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.preview_failed", Subject: current.SatelliteID, Severity: audit.Warning, Fields: map[string]any{"error": err.Error()}})
		return
	}
	_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.preview_recorded", Subject: current.SatelliteID, Fields: map[string]any{"preview_id": saved.ID, "from_revision": previous.Revision, "to_revision": current.Revision, "impacts": len(saved.Impacts)}})
}

// classify separates revision-affected passes by how far they have started.
func (r *Replanner) classify(value Pass) (ImpactClass, bool) {
	switch value.Phase {
	case Planned:
		if _, booked := r.timeline.ActiveForPass(value.ID); booked {
			return ClassBooked, true
		}
		return ClassNotStarted, true
	case Acquiring, Tracking, Draining:
		return ClassTracking, true
	default:
		return "", false
	}
}

// applyWindow replaces the pass window and its pointing commands.
func (r *Replanner) applyWindow(value Pass, window Window) error {
	if err := r.windows.Apply(value.ID, window); err != nil {
		return err
	}
	return r.replaceCommands(value, window)
}

// replaceCommands installs new pointing commands and republishes the window.
func (r *Replanner) replaceCommands(value Pass, window Window) error {
	points := []antenna.TrackPoint{
		{At: window.AOS.Add(-20 * time.Second), Azimuth: 120, Elevation: 10},
		{At: window.AOS, Azimuth: 135, Elevation: 20},
		{At: window.LOS, Azimuth: 240, Elevation: 8},
	}
	commands, err := antenna.BuildCommands(value.ID, value.AntennaID, window.Revision, points)
	if err != nil {
		return err
	}
	if err := r.queue.Replace(value.ID, window.Revision, commands); err != nil {
		return err
	}
	return r.passes.update(value.ID, func(pass *Pass) error {
		pass.Window = window
		return nil
	})
}

// Approve applies a pending review impact: the proposed window and pointing
// commands are installed, and a booked pass reservation moves with the window.
func (r *Replanner) Approve(impactID string) (Impact, error) {
	impact, found := r.previews.FindImpact(impactID)
	if !found {
		return Impact{}, fmt.Errorf("impact %s not found", impactID)
	}
	if impact.Disposition != PendingReview {
		return Impact{}, fmt.Errorf("impact %s is %s, not pending review", impactID, impact.Disposition)
	}
	value, found := r.passes.Get(impact.PassID)
	if !found {
		return Impact{}, fmt.Errorf("pass %s not found", impact.PassID)
	}
	if _, affected := r.classify(value); !affected {
		return Impact{}, fmt.Errorf("pass %s is %s and can no longer be replanned", value.ID, value.Phase)
	}
	if err := r.windows.Apply(value.ID, impact.ProposedWindow); err != nil {
		return Impact{}, err
	}
	if impact.Class == ClassBooked {
		if _, err := r.reservations.Refresh(value.ID); err != nil {
			_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.review_failed", Subject: value.ID, Severity: audit.Warning, Fields: map[string]any{"impact_id": impactID, "error": err.Error()}})
			return Impact{}, err
		}
	}
	if err := r.replaceCommands(value, impact.ProposedWindow); err != nil {
		_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.review_failed", Subject: value.ID, Severity: audit.Warning, Fields: map[string]any{"impact_id": impactID, "error": err.Error()}})
		return Impact{}, err
	}
	resolved, err := r.previews.ResolveImpact(impactID, Approved, "applied by operator review")
	if err != nil {
		return Impact{}, err
	}
	_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.review_approved", Subject: value.ID, Fields: map[string]any{"impact_id": impactID, "revision": impact.ProposedWindow.Revision}})
	return resolved, nil
}

// Reject keeps the current plan and closes the review impact.
func (r *Replanner) Reject(impactID, reason string) (Impact, error) {
	if reason == "" {
		reason = "rejected by operator"
	}
	resolved, err := r.previews.ResolveImpact(impactID, Rejected, reason)
	if err != nil {
		return Impact{}, err
	}
	_, _ = r.audit.Record(audit.Event{Component: "pass", Action: "replan.review_rejected", Subject: resolved.PassID, Fields: map[string]any{"impact_id": impactID, "reason": reason}})
	return resolved, nil
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
