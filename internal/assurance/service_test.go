package assurance_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"example.com/solo-0021-metrology-assurance-suite/internal/assurance"
	"example.com/solo-0021-metrology-assurance-suite/internal/contracts"
	"example.com/solo-0021-metrology-assurance-suite/internal/domain"
	"example.com/solo-0021-metrology-assurance-suite/internal/repository"
)

type fixture struct {
	service *assurance.Service
}

func newFixture() *fixture {
	return &fixture{service: assurance.NewService(repository.New())}
}

func (f *fixture) registerDevice(t *testing.T, tag string) string {
	t.Helper()
	device, err := f.service.RegisterDevice(context.Background(), contracts.RegisterDeviceRequest{
		AssetTag: tag,
		Model:    "Bench Meter",
		Room:     "Room A",
		Steward:  "Lin",
	})
	if err != nil {
		t.Fatalf("register device: %v", err)
	}
	return device.ID
}

func (f *fixture) openCase(t *testing.T, deviceID string) contracts.CaseResponse {
	t.Helper()
	view, err := f.service.OpenCase(context.Background(), contracts.OpenCaseRequest{
		DeviceID:  deviceID,
		Nominal:   25,
		Tolerance: 0.5,
		Steward:   "Qiao",
	})
	if err != nil {
		t.Fatalf("open case: %v", err)
	}
	return view
}

func (f *fixture) readyCase(t *testing.T, tag string) (string, string) {
	t.Helper()
	deviceID := f.registerDevice(t, tag)
	opened := f.openCase(t, deviceID)
	_, err := f.service.RecordMeasurement(context.Background(), opened.Case.ID, contracts.RecordMeasurementRequest{
		Observed:   25.2,
		Technician: "Rui",
	})
	if err != nil {
		t.Fatalf("record accepted measurement: %v", err)
	}
	return deviceID, opened.Case.ID
}

func (f *fixture) sealCase(t *testing.T, caseID string) {
	t.Helper()
	if _, err := f.service.SealCase(context.Background(), caseID, contracts.SealCaseRequest{
		Reviewer: "Verifier",
		Summary:  "accepted measurement sealed for review",
	}); err != nil {
		t.Fatalf("seal case: %v", err)
	}
}

// A sealed case must release the device so the same device can open a new
// case. Regression: occupancy was deleted under the case ID while the map is
// keyed by device ID, so the stale hold survived sealing.
func TestSealReleasesDeviceForNewCase(t *testing.T) {
	t.Parallel()

	f := newFixture()
	deviceID, caseID := f.readyCase(t, "MET-2001")
	f.sealCase(t, caseID)

	view, err := f.service.Case(context.Background(), caseID)
	if err != nil {
		t.Fatalf("read sealed case: %v", err)
	}
	if view.Case.State != domain.CaseSealed {
		t.Fatalf("sealed case state = %q", view.Case.State)
	}
	if view.Seal == nil {
		t.Fatal("sealed case has no evidence seal")
	}

	reopened, err := f.service.OpenCase(context.Background(), contracts.OpenCaseRequest{
		DeviceID:  deviceID,
		Nominal:   25,
		Tolerance: 0.5,
		Steward:   "Qiao",
	})
	if err != nil {
		t.Fatalf("open case after seal: %v", err)
	}
	if reopened.Case.State != domain.CaseAwaitingMeasurement {
		t.Fatalf("new case state = %q", reopened.Case.State)
	}
	if reopened.Case.ID == caseID {
		t.Fatal("new case reused the sealed case id")
	}
}

// A rejected open request must leave no occupancy, no case, no event and must
// not consume an ID. Regression: cloneOccupancies returned the committed map,
// so the pre-validation occupancy insert leaked into committed state.
func TestOpenCaseRejectedLeavesNoTrace(t *testing.T) {
	t.Parallel()

	f := newFixture()
	deviceID := f.registerDevice(t, "MET-2002")

	_, err := f.service.OpenCase(context.Background(), contracts.OpenCaseRequest{
		DeviceID:  deviceID,
		Nominal:   25,
		Tolerance: -1,
		Steward:   "Qiao",
	})
	if !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("expected invalid input, got %v", err)
	}

	view, err := f.service.OpenCase(context.Background(), contracts.OpenCaseRequest{
		DeviceID:  deviceID,
		Nominal:   25,
		Tolerance: 0.5,
		Steward:   "Qiao",
	})
	if err != nil {
		t.Fatalf("valid open after rejected open: %v", err)
	}
	if view.Case.ID != "case-0001" {
		t.Fatalf("rejected submission consumed an id: got %q", view.Case.ID)
	}
	if len(view.Events) != 1 || view.Events[0].Kind != "case_opened" {
		t.Fatalf("committed case has unexpected event history: %+v", view.Events)
	}
}

// Concurrent or retried open requests for one device collapse to a single
// committed case; every other request is rejected.
func TestConcurrentOpenCaseSingleWinner(t *testing.T) {
	t.Parallel()

	f := newFixture()
	deviceID := f.registerDevice(t, "MET-2003")

	const contenders = 8
	start := make(chan struct{})
	var wait sync.WaitGroup
	results := make([]error, contenders)
	ids := make([]string, contenders)
	for i := 0; i < contenders; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			view, err := f.service.OpenCase(context.Background(), contracts.OpenCaseRequest{
				DeviceID:  deviceID,
				Nominal:   25,
				Tolerance: 0.5,
				Steward:   "Qiao",
			})
			results[index] = err
			if err == nil {
				ids[index] = view.Case.ID
			}
		}(i)
	}
	close(start)
	wait.Wait()

	winners := 0
	winnerID := ""
	for i, err := range results {
		switch {
		case err == nil:
			winners++
			winnerID = ids[i]
		case errors.Is(err, domain.ErrActiveCaseExists):
		default:
			t.Fatalf("contender %d got unexpected error: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one winner, got %d", winners)
	}
	if winnerID != "case-0001" {
		t.Fatalf("winning case id = %q", winnerID)
	}
}

// Concurrent seal requests produce exactly one seal and release the device.
func TestConcurrentSealSingleWinnerAndDeviceReleased(t *testing.T) {
	t.Parallel()

	f := newFixture()
	deviceID, caseID := f.readyCase(t, "MET-2004")

	const contenders = 6
	start := make(chan struct{})
	var wait sync.WaitGroup
	results := make([]error, contenders)
	for i := 0; i < contenders; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, results[index] = f.service.SealCase(context.Background(), caseID, contracts.SealCaseRequest{
				Reviewer: "Verifier",
				Summary:  "accepted measurement sealed for review",
			})
		}(i)
	}
	close(start)
	wait.Wait()

	successes := 0
	for i, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrSealAlreadyCreated) || errors.Is(err, domain.ErrStateConflict):
		default:
			t.Fatalf("seal contender %d got unexpected error: %v", i, err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one seal, got %d", successes)
	}

	view, err := f.service.Case(context.Background(), caseID)
	if err != nil {
		t.Fatalf("read sealed case: %v", err)
	}
	if view.Seal == nil || len(view.Measurements) != 1 {
		t.Fatalf("sealed view inconsistent: %+v", view)
	}
	sealEvents := 0
	for _, event := range view.Events {
		if event.Kind == "case_sealed" {
			sealEvents++
		}
	}
	if sealEvents != 1 {
		t.Fatalf("expected one case_sealed event, got %d", sealEvents)
	}

	if _, err := f.service.OpenCase(context.Background(), contracts.OpenCaseRequest{
		DeviceID:  deviceID,
		Nominal:   25,
		Tolerance: 0.5,
		Steward:   "Qiao",
	}); err != nil {
		t.Fatalf("device not released after concurrent seal: %v", err)
	}
}

// A seal that fails validation changes nothing: case state, occupancy, seal
// records and audit history all stay as they were.
func TestSealRejectedLeavesNoHalfState(t *testing.T) {
	t.Parallel()

	f := newFixture()
	deviceID, caseID := f.readyCase(t, "MET-2005")

	if _, err := f.service.SealCase(context.Background(), caseID, contracts.SealCaseRequest{
		Reviewer: "",
		Summary:  "missing reviewer",
	}); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("expected invalid input for empty reviewer, got %v", err)
	}

	view, err := f.service.Case(context.Background(), caseID)
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	if view.Case.State != domain.CaseReviewReady {
		t.Fatalf("case state after failed seal = %q", view.Case.State)
	}
	if view.Seal != nil {
		t.Fatal("seal record created despite failed request")
	}
	for _, event := range view.Events {
		if event.Kind == "case_sealed" {
			t.Fatal("case_sealed event appended despite failed request")
		}
	}

	// The unfinished case still holds the device...
	if _, err := f.service.OpenCase(context.Background(), contracts.OpenCaseRequest{
		DeviceID: deviceID, Nominal: 25, Tolerance: 0.5, Steward: "Qiao",
	}); !errors.Is(err, domain.ErrActiveCaseExists) {
		t.Fatalf("expected active_case_exists while case is unsealed, got %v", err)
	}
	// ...and a corrected seal request still succeeds exactly once.
	f.sealCase(t, caseID)
}

// Sealing without an accepted measurement is rejected and changes nothing.
func TestSealWithoutAcceptedMeasurementRejected(t *testing.T) {
	t.Parallel()

	f := newFixture()
	deviceID := f.registerDevice(t, "MET-2006")
	opened := f.openCase(t, deviceID)

	_, err := f.service.SealCase(context.Background(), opened.Case.ID, contracts.SealCaseRequest{
		Reviewer: "Verifier",
		Summary:  "no measurement yet",
	})
	if !errors.Is(err, domain.ErrStateConflict) {
		t.Fatalf("expected state conflict, got %v", err)
	}
	view, err := f.service.Case(context.Background(), opened.Case.ID)
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	if view.Case.State != domain.CaseAwaitingMeasurement || view.Seal != nil {
		t.Fatalf("case altered by rejected seal: %+v", view)
	}
}

// Reopen validates actor and reason before changing state or occupancy.
func TestReopenRejectedLeavesStateUnchanged(t *testing.T) {
	t.Parallel()

	f := newFixture()
	deviceID := f.registerDevice(t, "MET-2007")
	opened := f.openCase(t, deviceID)
	if _, err := f.service.RecordMeasurement(context.Background(), opened.Case.ID, contracts.RecordMeasurementRequest{
		Observed: 26.5, Technician: "Rui",
	}); err != nil {
		t.Fatalf("record rejected measurement: %v", err)
	}

	_, err := f.service.ReopenCase(context.Background(), opened.Case.ID, contracts.ReopenCaseRequest{
		Actor: "", Reason: "",
	})
	if !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("expected invalid input, got %v", err)
	}

	view, err := f.service.Case(context.Background(), opened.Case.ID)
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	if view.Case.State != domain.CaseRecheckRequired {
		t.Fatalf("state after failed reopen = %q", view.Case.State)
	}
	for _, event := range view.Events {
		if event.Kind == "case_reopened" {
			t.Fatal("case_reopened event appended despite failed request")
		}
	}

	// Device still occupied by the unfinished case and a valid reopen works.
	if _, err := f.service.OpenCase(context.Background(), contracts.OpenCaseRequest{
		DeviceID: deviceID, Nominal: 25, Tolerance: 0.5, Steward: "Qiao",
	}); !errors.Is(err, domain.ErrActiveCaseExists) {
		t.Fatalf("expected device to stay occupied, got %v", err)
	}
	reopened, err := f.service.ReopenCase(context.Background(), opened.Case.ID, contracts.ReopenCaseRequest{
		Actor: "Lin", Reason: "measurement setup was replaced",
	})
	if err != nil {
		t.Fatalf("valid reopen: %v", err)
	}
	if reopened.Case.State != domain.CaseAwaitingMeasurement {
		t.Fatalf("reopened state = %q", reopened.Case.State)
	}
}

// An invalid measurement appends nothing and leaves the case awaiting.
func TestMeasurementRejectedLeavesNoTrace(t *testing.T) {
	t.Parallel()

	f := newFixture()
	deviceID := f.registerDevice(t, "MET-2008")
	opened := f.openCase(t, deviceID)

	_, err := f.service.RecordMeasurement(context.Background(), opened.Case.ID, contracts.RecordMeasurementRequest{
		Observed: 25.2, Technician: "",
	})
	if !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("expected invalid input, got %v", err)
	}
	view, err := f.service.Case(context.Background(), opened.Case.ID)
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	if view.Case.State != domain.CaseAwaitingMeasurement {
		t.Fatalf("state after invalid measurement = %q", view.Case.State)
	}
	if len(view.Measurements) != 0 {
		t.Fatalf("measurement appended despite validation failure: %+v", view.Measurements)
	}

	// Device remains occupied by the still-open case.
	if _, err := f.service.OpenCase(context.Background(), contracts.OpenCaseRequest{
		DeviceID: deviceID, Nominal: 25, Tolerance: 0.5, Steward: "Qiao",
	}); !errors.Is(err, domain.ErrActiveCaseExists) {
		t.Fatalf("expected device to stay occupied, got %v", err)
	}
}

// A sealed case rejects later measurements and appends nothing.
func TestMeasurementAfterSealRejected(t *testing.T) {
	t.Parallel()

	f := newFixture()
	_, caseID := f.readyCase(t, "MET-2009")
	f.sealCase(t, caseID)

	_, err := f.service.RecordMeasurement(context.Background(), caseID, contracts.RecordMeasurementRequest{
		Observed: 25.0, Technician: "Rui",
	})
	if !errors.Is(err, domain.ErrStateConflict) {
		t.Fatalf("expected state conflict, got %v", err)
	}
	view, err := f.service.Case(context.Background(), caseID)
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	if len(view.Measurements) != 1 {
		t.Fatalf("sealed case measurements changed: %d", len(view.Measurements))
	}
}
