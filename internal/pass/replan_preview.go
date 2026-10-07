package pass

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"orbitlink/internal/store"
)

// ImpactClass separates revision-affected passes by how far they have started.
type ImpactClass string

const (
	ClassNotStarted ImpactClass = "not_started"
	ClassBooked     ImpactClass = "booked"
	ClassTracking   ImpactClass = "tracking"
)

// Disposition records what the replanner did, or may still do, with an impact.
type Disposition string

const (
	AutoApplied   Disposition = "auto_applied"
	PendingReview Disposition = "pending_review"
	Approved      Disposition = "approved"
	Rejected      Disposition = "rejected"
	Superseded    Disposition = "superseded"
)

type Impact struct {
	ID             string      `json:"id"`
	PreviewID      string      `json:"preview_id"`
	PassID         string      `json:"pass_id"`
	SatelliteID    string      `json:"satellite_id"`
	Class          ImpactClass `json:"class"`
	PreviousWindow Window      `json:"previous_window"`
	ProposedWindow Window      `json:"proposed_window"`
	Disposition    Disposition `json:"disposition"`
	Reason         string      `json:"reason,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	ResolvedAt     time.Time   `json:"resolved_at,omitempty"`
}

type Preview struct {
	ID           string    `json:"id"`
	SatelliteID  string    `json:"satellite_id"`
	FromRevision int64     `json:"from_revision"`
	ToRevision   int64     `json:"to_revision"`
	CreatedAt    time.Time `json:"created_at"`
	Impacts      []Impact  `json:"impacts"`
}

// PreviewStore keeps every reschedule preview and its review state durable.
type PreviewStore struct {
	mu       sync.RWMutex
	path     string
	previews []Preview
}

func NewPreviewStore(paths store.Paths) (*PreviewStore, error) {
	previewStore := &PreviewStore{path: paths.Snapshot("replan-previews")}
	found, err := store.ReadJSON(previewStore.path, &previewStore.previews)
	if err != nil {
		return nil, err
	}
	if !found {
		previewStore.previews = make([]Preview, 0)
	}
	return previewStore, nil
}

func (s *PreviewStore) Save(preview Preview) (Preview, error) {
	if preview.SatelliteID == "" || preview.ToRevision < 1 {
		return Preview{}, fmt.Errorf("preview satellite and target revision are required")
	}
	if preview.ID == "" {
		preview.ID = uuid.NewString()
	}
	if preview.CreatedAt.IsZero() {
		preview.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range preview.Impacts {
		if preview.Impacts[index].ID == "" {
			preview.Impacts[index].ID = uuid.NewString()
		}
		preview.Impacts[index].PreviewID = preview.ID
		if preview.Impacts[index].CreatedAt.IsZero() {
			preview.Impacts[index].CreatedAt = preview.CreatedAt
		}
	}
	s.previews = append(s.previews, preview)
	if err := s.flushLocked(); err != nil {
		s.previews = s.previews[:len(s.previews)-1]
		return Preview{}, err
	}
	return preview, nil
}

func (s *PreviewStore) List() []Preview {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := append([]Preview(nil), s.previews...)
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

// Impacts flattens every preview so operators can scan impacts directly.
func (s *PreviewStore) Impacts() []Impact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var values []Impact
	for _, preview := range s.previews {
		values = append(values, preview.Impacts...)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

func (s *PreviewStore) FindImpact(impactID string) (Impact, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, preview := range s.previews {
		for _, impact := range preview.Impacts {
			if impact.ID == impactID {
				return impact, true
			}
		}
	}
	return Impact{}, false
}

// ResolveImpact marks a pending impact with a terminal review disposition.
func (s *PreviewStore) ResolveImpact(impactID string, disposition Disposition, reason string) (Impact, error) {
	if disposition != Approved && disposition != Rejected {
		return Impact{}, fmt.Errorf("review disposition must be approved or rejected")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for previewIndex := range s.previews {
		for impactIndex := range s.previews[previewIndex].Impacts {
			impact := &s.previews[previewIndex].Impacts[impactIndex]
			if impact.ID != impactID {
				continue
			}
			if impact.Disposition != PendingReview {
				return Impact{}, fmt.Errorf("impact %s is %s, not pending review", impactID, impact.Disposition)
			}
			impact.Disposition = disposition
			impact.Reason = reason
			impact.ResolvedAt = time.Now().UTC()
			if err := s.flushLocked(); err != nil {
				return Impact{}, err
			}
			return *impact, nil
		}
	}
	return Impact{}, fmt.Errorf("impact %s not found", impactID)
}

// SupersedePending retires pending impacts for a satellite replaced by a newer preview.
func (s *PreviewStore) SupersedePending(satelliteID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for previewIndex := range s.previews {
		for impactIndex := range s.previews[previewIndex].Impacts {
			impact := &s.previews[previewIndex].Impacts[impactIndex]
			if impact.SatelliteID == satelliteID && impact.Disposition == PendingReview {
				impact.Disposition = Superseded
				impact.Reason = "replaced by a newer ephemeris revision"
				impact.ResolvedAt = time.Now().UTC()
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return s.flushLocked()
}

func (s *PreviewStore) flushLocked() error {
	return store.WriteJSON(s.path, s.previews)
}
