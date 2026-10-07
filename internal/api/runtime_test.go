package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stationclock "orbitlink/internal/clock"
	passsvc "orbitlink/internal/pass"
	"orbitlink/internal/receiver"
	"orbitlink/internal/rf"
	"orbitlink/internal/satellite"
	"orbitlink/internal/weather"
)

func TestOperationsPagesAndAPIsUseRuntimeState(t *testing.T) {
	runtime := newTestRuntime(t)
	router, err := NewRouter(runtime)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	for _, target := range []string{"/passes", "/antennas", "/rf-chains", "/recordings", "/replan", "/healthz", "/api/passes", "/api/antennas", "/api/rf-chains", "/api/recordings", "/api/replan/previews", "/api/replan/impacts"} {
		response, err := server.Client().Get(server.URL + target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned %d", target, response.StatusCode)
		}
		_ = response.Body.Close()
	}
}

func TestPassRevisionAcquisitionAndRecordingFlow(t *testing.T) {
	runtime := newTestRuntime(t)
	value := runtime.Passes.List()[0]
	if _, err := runtime.RF.Apply(rf.Plan{PassID: value.ID, ChainID: "RF-01", CenterHz: 2_245_000_000, DopplerHz: 12_000, Polarization: rf.RightCircular}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.RF.Apply(rf.Plan{PassID: value.ID, ChainID: "RF-02", CenterHz: 2_245_000_000, DopplerHz: 12_050, Polarization: rf.RightCircular}); err != nil {
		t.Fatal(err)
	}
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
	decision, err := runtime.Passes.AcceptDiversity(value.ID, results)
	if err != nil || !decision.Ready {
		t.Fatalf("diversity did not converge: %#v %v", decision, err)
	}
	active, found := runtime.Recorder.ActiveForPass(value.ID)
	if !found {
		t.Fatal("tracking did not start a recorder")
	}
	session, err := runtime.NewDecoderSession(value.ID, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.OnLock(attempt.ID); err != nil {
		t.Fatal(err)
	}
	if written, err := session.Feed(attempt.ID, make([]byte, 128)); err != nil || written != 2 {
		t.Fatalf("decoder feed wrote %d frames: %v", written, err)
	}
	if _, err := runtime.Recorder.Drain(context.Background(), active.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Recorder.Seal(active.ID, "integration complete"); err != nil {
		t.Fatal(err)
	}
}

func TestEphemerisClockAndWindSafetyFlow(t *testing.T) {
	runtime := newTestRuntime(t)
	value := runtime.Passes.List()[0]
	previous, _ := runtime.Ephemerides.Current(value.SatelliteID)
	if _, err := runtime.Ephemerides.Import(satellite.Ephemeris{SatelliteID: value.SatelliteID, Revision: previous.Revision + 1, Epoch: time.Now().UTC(), Line1: "revised orbital element one", Line2: "revised orbital element two"}); err != nil {
		t.Fatal(err)
	}
	// The seeded pass holds a reservation, so the revision waits for manual review.
	commands := runtime.Commands.List(value.ID)
	if len(commands) == 0 || commands[len(commands)-1].Revision != previous.Revision {
		t.Fatal("booked pass commands changed before review")
	}
	impacts := runtime.ReplanPreviews.Impacts()
	if len(impacts) != 1 || impacts[0].Class != passsvc.ClassBooked || impacts[0].Disposition != passsvc.PendingReview {
		t.Fatalf("expected one booked pending-review impact: %#v", impacts)
	}
	if _, err := runtime.Replanner.Approve(impacts[0].ID); err != nil {
		t.Fatal(err)
	}
	commands = runtime.Commands.List(value.ID)
	if len(commands) == 0 || commands[len(commands)-1].Revision != previous.Revision+1 {
		t.Fatal("revised pointing commands were not queued after review approval")
	}
	if _, err := runtime.Clock.Apply(stationclock.Sample{ObservedAt: time.Now().UTC(), Offset: 2 * time.Second, Source: "gps", Quality: "locked"}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.Clock.Corrections()) != 1 {
		t.Fatal("clock correction was not persisted")
	}
	if err := runtime.RF.EnableTransmit("RF-01"); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Weather.Handle(context.Background(), weather.WindSample{StationID: "station", GustMetersPerSecond: 25, ObservedAt: time.Now().UTC()}, value.ID, value.AntennaID, "RF-01")
	if err != nil || !result.InhibitConfirmed || !result.Stowed {
		t.Fatalf("wind safety did not converge: %#v %v", result, err)
	}
}

func TestReplanPreviewClassifiesImpacts(t *testing.T) {
	runtime := newTestRuntime(t)
	booked := runtime.Passes.List()[0]
	now := runtime.Clock.Now()
	notStarted, err := runtime.Passes.Create(passsvc.CreateRequest{SatelliteID: "SAT-A12", AntennaID: "ANT-02", RFChains: []string{"RF-01"}, RequiredReceiverChains: []string{"RF-01"}, AOS: now.Add(20 * time.Minute), LOS: now.Add(28 * time.Minute), Revision: 1, OwnerStation: "north"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.WindowRecompute.Apply(notStarted.ID, notStarted.Window); err != nil {
		t.Fatal(err)
	}
	tracking, err := runtime.Passes.Create(passsvc.CreateRequest{SatelliteID: "SAT-A12", AntennaID: "ANT-03", RFChains: []string{"RF-02"}, RequiredReceiverChains: []string{"RF-02"}, AOS: now.Add(30 * time.Minute), LOS: now.Add(38 * time.Minute), Revision: 1, OwnerStation: "north"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := runtime.Attempts.Begin(tracking.ID, "RF-02")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Passes.BeginAcquisition(tracking.ID, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Passes.ApplyAcquisitionResult(receiver.AcquisitionResult{PassID: tracking.ID, AttemptID: attempt.ID, ChainID: "RF-02", Locked: true, Synced: true, Epoch: 3}); err != nil {
		t.Fatal(err)
	}

	if _, err := runtime.Ephemerides.Import(satellite.Ephemeris{SatelliteID: "SAT-A12", Revision: 2, Epoch: time.Now().UTC(), Line1: "revision two line one", Line2: "revision two line two"}); err != nil {
		t.Fatal(err)
	}
	previews := runtime.ReplanPreviews.List()
	if len(previews) != 1 || len(previews[0].Impacts) != 3 {
		t.Fatalf("expected one preview with three impacts: %#v", previews)
	}
	byPass := make(map[string]passsvc.Impact, 3)
	for _, impact := range previews[0].Impacts {
		byPass[impact.PassID] = impact
	}

	autoImpact := byPass[notStarted.ID]
	if autoImpact.Class != passsvc.ClassNotStarted || autoImpact.Disposition != passsvc.AutoApplied {
		t.Fatalf("not-started pass was not auto applied: %#v", autoImpact)
	}
	updated, _ := runtime.Passes.Get(notStarted.ID)
	if !updated.Window.AOS.Equal(notStarted.Window.AOS.Add(time.Second)) || updated.Window.Revision != 2 {
		t.Fatalf("not-started window was not shifted: %#v", updated.Window)
	}
	commands := runtime.Commands.List(notStarted.ID)
	if len(commands) == 0 || commands[len(commands)-1].Revision != 2 {
		t.Fatal("not-started pointing commands were not replaced")
	}

	bookedImpact := byPass[booked.ID]
	if bookedImpact.Class != passsvc.ClassBooked || bookedImpact.Disposition != passsvc.PendingReview {
		t.Fatalf("booked pass did not enter review: %#v", bookedImpact)
	}
	unchanged, _ := runtime.Passes.Get(booked.ID)
	if unchanged.Window.Revision != 1 {
		t.Fatalf("booked window changed before review: %#v", unchanged.Window)
	}

	trackingImpact := byPass[tracking.ID]
	if trackingImpact.Class != passsvc.ClassTracking || trackingImpact.Disposition != passsvc.PendingReview {
		t.Fatalf("tracking pass did not enter review: %#v", trackingImpact)
	}

	if _, err := runtime.Replanner.Reject(trackingImpact.ID, "keep live contact on current elements"); err != nil {
		t.Fatal(err)
	}
	afterReject, _ := runtime.Passes.Get(tracking.ID)
	if afterReject.Window.Revision != 1 {
		t.Fatal("rejected review still changed the window")
	}

	reservationBefore, found := runtime.Timeline.ActiveForPass(booked.ID)
	if !found {
		t.Fatal("booked pass lost its reservation")
	}
	if _, err := runtime.Replanner.Approve(bookedImpact.ID); err != nil {
		t.Fatal(err)
	}
	rescheduled, _ := runtime.Passes.Get(booked.ID)
	if rescheduled.Window.Revision != 2 {
		t.Fatalf("approved window was not applied: %#v", rescheduled.Window)
	}
	reservationAfter, found := runtime.Timeline.ActiveForPass(booked.ID)
	if !found || reservationAfter.ReservationID == reservationBefore.ReservationID {
		t.Fatal("approved review did not move the reservation")
	}
	if !reservationAfter.Start.Equal(rescheduled.Window.AOS.Add(-30*time.Second)) || !reservationAfter.End.Equal(rescheduled.Window.LOS) {
		t.Fatalf("reservation does not cover the new window: %#v", reservationAfter)
	}

	// A newer revision supersedes impacts still waiting for review.
	if _, err := runtime.Ephemerides.Import(satellite.Ephemeris{SatelliteID: "SAT-A12", Revision: 3, Epoch: time.Now().UTC(), Line1: "revision three line one", Line2: "revision three line two"}); err != nil {
		t.Fatal(err)
	}
	stale := pendingImpactFor(t, runtime, tracking.ID)
	if _, err := runtime.Ephemerides.Import(satellite.Ephemeris{SatelliteID: "SAT-A12", Revision: 4, Epoch: time.Now().UTC(), Line1: "revision four line one", Line2: "revision four line two"}); err != nil {
		t.Fatal(err)
	}
	superseded, found := runtime.ReplanPreviews.FindImpact(stale.ID)
	if !found || superseded.Disposition != passsvc.Superseded {
		t.Fatalf("stale review was not superseded: %#v", superseded)
	}
	if current := pendingImpactFor(t, runtime, tracking.ID); current.ID == stale.ID {
		t.Fatal("newer revision did not raise a fresh review impact")
	}
}

func TestReplanPreviewAPI(t *testing.T) {
	runtime := newTestRuntime(t)
	router, err := NewRouter(runtime)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()

	response := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/ephemeris", `{"satellite_id":"SAT-A12","revision":2,"epoch":"2026-10-06T00:00:00Z","line1":"revised line one","line2":"revised line two"}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("ephemeris import returned %d", response.StatusCode)
	}
	_ = response.Body.Close()

	response, err = server.Client().Get(server.URL + "/api/replan/previews")
	if err != nil {
		t.Fatal(err)
	}
	var previews struct {
		Items []passsvc.Preview `json:"items"`
	}
	decodeBody(t, response, &previews)
	if len(previews.Items) != 1 || len(previews.Items[0].Impacts) != 1 {
		t.Fatalf("expected one preview with one impact: %#v", previews.Items)
	}
	impact := previews.Items[0].Impacts[0]
	if impact.Class != passsvc.ClassBooked || impact.Disposition != passsvc.PendingReview {
		t.Fatalf("unexpected impact classification: %#v", impact)
	}
	if impact.ProposedWindow.Revision != 2 || !impact.ProposedWindow.AOS.Equal(impact.PreviousWindow.AOS.Add(time.Second)) {
		t.Fatalf("unexpected proposed window: %#v", impact.ProposedWindow)
	}

	response, err = server.Client().Get(server.URL + "/api/replan/impacts")
	if err != nil {
		t.Fatal(err)
	}
	var impacts struct {
		Items []passsvc.Impact `json:"items"`
	}
	decodeBody(t, response, &impacts)
	if len(impacts.Items) != 1 || impacts.Items[0].ID != impact.ID {
		t.Fatalf("impact feed does not match preview: %#v", impacts.Items)
	}

	response = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/replan/reviews/"+impact.ID+"/reject", `{"reason":"hold for coordination"}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reject returned %d", response.StatusCode)
	}
	var rejected passsvc.Impact
	decodeBody(t, response, &rejected)
	if rejected.Disposition != passsvc.Rejected || rejected.Reason != "hold for coordination" {
		t.Fatalf("review was not rejected: %#v", rejected)
	}

	response = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/replan/reviews/"+impact.ID+"/approve", `{}`)
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("re-resolving a closed review returned %d", response.StatusCode)
	}
	_ = response.Body.Close()
}

func pendingImpactFor(t *testing.T, runtime *Runtime, passID string) passsvc.Impact {
	t.Helper()
	for _, impact := range runtime.ReplanPreviews.Impacts() {
		if impact.PassID == passID && impact.Disposition == passsvc.PendingReview {
			return impact
		}
	}
	t.Fatalf("no pending review impact for pass %s", passID)
	return passsvc.Impact{}
}

func TestRouterRejectsOverlappingReservation(t *testing.T) {
	runtime := newTestRuntime(t)
	first := runtime.Passes.List()[0]
	second, err := runtime.Passes.Create(passsvc.CreateRequest{SatelliteID: "SAT-C07", AntennaID: first.AntennaID, RFChains: []string{"RF-03"}, RequiredReceiverChains: []string{"RF-03"}, AOS: first.Window.AOS.Add(time.Minute), LOS: first.Window.LOS.Add(time.Minute), Revision: 1, OwnerStation: "north"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.WindowRecompute.Apply(second.ID, second.Window); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Reservations.ReservePass(second.ID, second.AntennaID); err == nil {
		t.Fatal("overlapping reservation unexpectedly succeeded")
	}
}

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	runtime, err := NewRuntime(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func decodeBody(t *testing.T, response *http.Response, value any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(value); err != nil {
		t.Fatal(err)
	}
}

func requestJSON(t *testing.T, client *http.Client, method, target, payload string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, target, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
