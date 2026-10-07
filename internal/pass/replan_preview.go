package pass

import "time"

// TaskCategory separates revision impacts by how far a pass has progressed.
type TaskCategory string

const (
	TaskNotStarted TaskCategory = "not_started"
	TaskBooked     TaskCategory = "booked"
	TaskTracking   TaskCategory = "tracking"
)

// Disposition decides who owns the next step for an impacted pass.
type Disposition string

const (
	DispositionAuto   Disposition = "auto_reschedule"
	DispositionReview Disposition = "manual_review"
)

type ReviewStatus string

const (
	ReviewPending    ReviewStatus = "pending"
	ReviewApproved   ReviewStatus = "approved"
	ReviewRejected   ReviewStatus = "rejected"
	ReviewSuperseded ReviewStatus = "superseded"
)

// ImpactItem is one pass inside a revision preview: the window it had, the
// window the revision proposes, and what the replanner did about it.
type ImpactItem struct {
	ID             string       `json:"id"`
	PreviewID      string       `json:"preview_id"`
	SatelliteID    string       `json:"satellite_id"`
	PassID         string       `json:"pass_id"`
	AntennaID      string       `json:"antenna_id"`
	Phase          Phase        `json:"phase"`
	Category       TaskCategory `json:"category"`
	Disposition    Disposition  `json:"disposition"`
	ReservationID  string       `json:"reservation_id,omitempty"`
	PreviousWindow Window       `json:"previous_window"`
	ProposedWindow Window       `json:"proposed_window"`
	ShiftSeconds   int64        `json:"shift_seconds"`
	Applied        bool         `json:"applied"`
	ReviewStatus   ReviewStatus `json:"review_status,omitempty"`
	ReviewNote     string       `json:"review_note,omitempty"`
}

// RevisionPreview is the viewable impact record of one ephemeris revision.
type RevisionPreview struct {
	ID               string       `json:"id"`
	SatelliteID      string       `json:"satellite_id"`
	PreviousRevision int64        `json:"previous_revision"`
	CurrentRevision  int64        `json:"current_revision"`
	CreatedAt        time.Time    `json:"created_at"`
	Items            []ImpactItem `json:"items"`
}

type PreviewSummary struct {
	Total           int `json:"total"`
	AutoRescheduled int `json:"auto_rescheduled"`
	PendingReview   int `json:"pending_review"`
}

func (p RevisionPreview) Summary() PreviewSummary {
	summary := PreviewSummary{Total: len(p.Items)}
	for _, item := range p.Items {
		switch {
		case item.Disposition == DispositionAuto && item.Applied:
			summary.AutoRescheduled++
		case item.ReviewStatus == ReviewPending:
			summary.PendingReview++
		}
	}
	return summary
}

// classify reports whether a pass is impacted by a revision and how far the
// task has progressed: unreserved passes have not started, reserved passes are
// booked, and passes past planning are live contacts.
func classify(value Pass) (TaskCategory, bool) {
	switch value.Phase {
	case Planned:
		if value.ReservationID != "" {
			return TaskBooked, true
		}
		return TaskNotStarted, true
	case Acquiring, Tracking, Draining:
		return TaskTracking, true
	default:
		return "", false
	}
}
