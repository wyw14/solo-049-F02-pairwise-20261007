package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	passsvc "orbitlink/internal/pass"
	"orbitlink/internal/receiver"
	"orbitlink/internal/satellite"
)

func importRevision(t *testing.T, runtime *Runtime, satelliteID string, revision int64) {
	t.Helper()
	if _, err := runtime.Ephemerides.Import(satellite.Ephemeris{SatelliteID: satelliteID, Revision: revision, Epoch: time.Now().UTC(), Line1: "revised orbital element one", Line2: "revised orbital element two"}); err != nil {
		t.Fatal(err)
	}
}

func TestReplanPreviewAutoReschedulesOnlyNotStartedPasses(t *testing.T) {
	runtime := newTestRuntime(t)
	booked := runtime.Passes.List()[0]
	if booked.ReservationID == "" {
		t.Fatal("seeded pass should carry a reservation")
	}
	open, err := runtime.Passes.Create(passsvc.CreateRequest{SatelliteID: booked.SatelliteID, AntennaID: "ANT-02", RFChains: []string{"RF-03"}, RequiredReceiverChains: []string{"RF-03"}, AOS: booked.Window.AOS.Add(time.Hour), LOS: booked.Window.LOS.Add(time.Hour), Revision: 1, OwnerStation: "north"})
	if err != nil {
		t.Fatal(err)
	}
	importRevision(t, runtime, booked.SatelliteID, 2)

	rescheduled, found := runtime.Passes.Get(open.ID)
	if !found || rescheduled.Window.Revision != 2 {
		t.Fatalf("not-started pass window was not auto-rescheduled: %#v", rescheduled.Window)
	}
	if !rescheduled.Window.AOS.Equal(open.Window.AOS.Add(time.Second)) || !rescheduled.Window.LOS.Equal(open.Window.LOS.Add(time.Second)) {
		t.Fatalf("not-started pass window shift mismatch: %#v", rescheduled.Window)
	}
	commands := runtime.Commands.List(open.ID)
	if len(commands) == 0 || commands[len(commands)-1].Revision != 2 {
		t.Fatal("not-started pass pointing commands were not replaced")
	}

	stillBooked, _ := runtime.Passes.Get(booked.ID)
	if stillBooked.Window.Revision != 1 {
		t.Fatalf("booked pass window changed without review: %#v", stillBooked.Window)
	}
	for _, command := range runtime.Commands.List(booked.ID) {
		if !command.Retired && command.Revision != 1 {
			t.Fatalf("booked pass commands moved to revision %d without review", command.Revision)
		}
	}

	previews := runtime.Replanner.Previews()
	if len(previews) != 1 || len(previews[0].Items) != 2 {
		t.Fatalf("expected one preview with two impacts, got %#v", previews)
	}
	summary := previews[0].Summary()
	if summary.AutoRescheduled != 1 || summary.PendingReview != 1 {
		t.Fatalf("preview summary mismatch: %#v", summary)
	}
	reviews := runtime.Replanner.PendingReviews()
	if len(reviews) != 1 || reviews[0].PassID != booked.ID || reviews[0].Category != passsvc.TaskBooked {
		t.Fatalf("expected one booked review item, got %#v", reviews)
	}
	if reviews[0].PreviousWindow.Revision != 1 || reviews[0].ProposedWindow.Revision != 2 || reviews[0].ShiftSeconds != 1 {
		t.Fatalf("review item impact detail mismatch: %#v", reviews[0])
	}
}

func TestReplanPreviewRoutesTrackingPassToManualReview(t *testing.T) {
	runtime := newTestRuntime(t)
	value := runtime.Passes.List()[0]
	attempt, err := runtime.Attempts.Begin(value.ID, "RF-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Passes.BeginAcquisition(value.ID, attempt.ID); err != nil {
		t.Fatal(err)
	}
	results := []receiver.AcquisitionResult{
		{PassID: value.ID, AttemptID: attempt.ID, ChainID: "RF-01", Locked: true, Synced: true, Epoch: 7},
		{PassID: value.ID, AttemptID: attempt.ID, ChainID: "RF-02", Locked: true, Synced: true, Epoch: 7},
	}
	if decision, err := runtime.Passes.AcceptDiversity(value.ID, results); err != nil || !decision.Ready {
		t.Fatalf("diversity did not converge: %#v %v", decision, err)
	}
	importRevision(t, runtime, value.SatelliteID, 2)

	reviews := runtime.Replanner.PendingReviews()
	if len(reviews) != 1 || reviews[0].Category != passsvc.TaskTracking {
		t.Fatalf("tracking pass did not enter manual review: %#v", reviews)
	}
	current, _ := runtime.Passes.Get(value.ID)
	if current.Phase != passsvc.Tracking || current.Window.Revision != 1 {
		t.Fatalf("tracking pass was disturbed by revision: %#v", current)
	}
}

func TestReplanReviewApproveAppliesProposalAndRefreshesReservation(t *testing.T) {
	runtime := newTestRuntime(t)
	booked := runtime.Passes.List()[0]
	importRevision(t, runtime, booked.SatelliteID, 2)
	reviews := runtime.Replanner.PendingReviews()
	if len(reviews) != 1 {
		t.Fatalf("expected one pending review, got %#v", reviews)
	}
	approved, err := runtime.Replanner.Approve(reviews[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.ReviewStatus != passsvc.ReviewApproved || !approved.Applied {
		t.Fatalf("review item was not marked approved: %#v", approved)
	}
	updated, _ := runtime.Passes.Get(booked.ID)
	if updated.Window.Revision != 2 {
		t.Fatalf("approved pass window was not applied: %#v", updated.Window)
	}
	if updated.ReservationID == "" || updated.ReservationID == booked.ReservationID {
		t.Fatalf("approved pass reservation was not refreshed: %#v", updated.ReservationID)
	}
	commands := runtime.Commands.List(booked.ID)
	if len(commands) == 0 || commands[len(commands)-1].Revision != 2 {
		t.Fatal("approved pass pointing commands were not replaced")
	}
	if _, err := runtime.Replanner.Approve(approved.ID); err == nil {
		t.Fatal("approving an already approved item unexpectedly succeeded")
	}
}

func TestReplanReviewRejectKeepsCurrentWindow(t *testing.T) {
	runtime := newTestRuntime(t)
	booked := runtime.Passes.List()[0]
	importRevision(t, runtime, booked.SatelliteID, 2)
	reviews := runtime.Replanner.PendingReviews()
	if len(reviews) != 1 {
		t.Fatalf("expected one pending review, got %#v", reviews)
	}
	rejected, err := runtime.Replanner.Reject(reviews[0].ID, "keep current contact plan")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.ReviewStatus != passsvc.ReviewRejected || rejected.ReviewNote != "keep current contact plan" {
		t.Fatalf("review item was not marked rejected: %#v", rejected)
	}
	updated, _ := runtime.Passes.Get(booked.ID)
	if updated.Window.Revision != 1 {
		t.Fatalf("rejected pass window changed: %#v", updated.Window)
	}
	if len(runtime.Replanner.PendingReviews()) != 0 {
		t.Fatal("rejected item still pending")
	}
}

func TestReplanPreviewSupersedesStaleReviewItems(t *testing.T) {
	runtime := newTestRuntime(t)
	booked := runtime.Passes.List()[0]
	importRevision(t, runtime, booked.SatelliteID, 2)
	stale := runtime.Replanner.PendingReviews()
	if len(stale) != 1 {
		t.Fatalf("expected one pending review, got %#v", stale)
	}
	importRevision(t, runtime, booked.SatelliteID, 3)
	reviews := runtime.Replanner.PendingReviews()
	if len(reviews) != 1 || reviews[0].ID == stale[0].ID {
		t.Fatalf("stale review item was not superseded: %#v", reviews)
	}
	if reviews[0].ProposedWindow.Revision != 3 {
		t.Fatalf("fresh review item targets wrong revision: %#v", reviews[0])
	}
	if _, err := runtime.Replanner.Approve(stale[0].ID); err == nil {
		t.Fatal("approving a superseded item unexpectedly succeeded")
	}
}

func TestReplanPreviewAPIs(t *testing.T) {
	runtime := newTestRuntime(t)
	router, err := NewRouter(runtime)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	booked := runtime.Passes.List()[0]
	importRevision(t, runtime, booked.SatelliteID, 2)

	response, err := server.Client().Get(server.URL + "/api/replan/previews")
	if err != nil {
		t.Fatal(err)
	}
	var previews struct {
		Items []struct {
			ID    string `json:"id"`
			Items []struct {
				ID          string               `json:"id"`
				Category    passsvc.TaskCategory `json:"category"`
				Disposition passsvc.Disposition  `json:"disposition"`
			} `json:"items"`
			Summary passsvc.PreviewSummary `json:"summary"`
		} `json:"items"`
	}
	decodeBody(t, response, &previews)
	if len(previews.Items) != 1 || previews.Items[0].Summary.PendingReview != 1 {
		t.Fatalf("preview API mismatch: %#v", previews)
	}
	if previews.Items[0].Items[0].Category != passsvc.TaskBooked || previews.Items[0].Items[0].Disposition != passsvc.DispositionReview {
		t.Fatalf("preview item mismatch: %#v", previews.Items[0].Items[0])
	}
	itemID := previews.Items[0].Items[0].ID

	response, err = server.Client().Get(server.URL + "/api/replan/reviews")
	if err != nil {
		t.Fatal(err)
	}
	var reviews struct {
		Items []passsvc.ImpactItem `json:"items"`
	}
	decodeBody(t, response, &reviews)
	if len(reviews.Items) != 1 || reviews.Items[0].ID != itemID {
		t.Fatalf("reviews API mismatch: %#v", reviews)
	}

	response = requestJSON(t, server.Client(), http.MethodPost, fmt.Sprintf("%s/api/replan/reviews/%s/approve", server.URL, itemID), "{}")
	var approved passsvc.ImpactItem
	decodeBody(t, response, &approved)
	if approved.ReviewStatus != passsvc.ReviewApproved {
		t.Fatalf("approve API mismatch: %#v", approved)
	}

	response, err = server.Client().Get(server.URL + "/api/replan/reviews")
	if err != nil {
		t.Fatal(err)
	}
	decodeBody(t, response, &reviews)
	if len(reviews.Items) != 0 {
		t.Fatalf("approved item still pending: %#v", reviews)
	}
}
