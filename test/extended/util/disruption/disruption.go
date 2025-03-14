package disruption

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/openshift/origin/pkg/monitor/monitorapi"
	monitorserialization "github.com/openshift/origin/pkg/monitor/serialization"

	g "github.com/onsi/ginkgo"

	"k8s.io/kubernetes/test/e2e/chaosmonkey"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/ginkgowrapper"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/upgrades"
	"k8s.io/kubernetes/test/utils/junit"
)

// testWithDisplayName is implemented by tests that want more descriptive test names
// than Name() (which must be namespace safe) allows.
type testWithDisplayName interface {
	DisplayName() string
}

// additionalTest is a test summary type that allows disruption suites to report
// extra JUnit outcomes for parts of a test.
type additionalTest struct {
	Name     string
	Failure  string
	Duration time.Duration
}

func (s additionalTest) PrintHumanReadable() string { return fmt.Sprintf("%s: %s", s.Name, s.Failure) }
func (s additionalTest) SummaryKind() string        { return "AdditionalTest" }
func (s additionalTest) PrintJSON() string          { data, _ := json.Marshal(s); return string(data) }

// flakeSummary is a test summary type that allows upgrades to report violations
// without failing the upgrade test.
type flakeSummary string

func (s flakeSummary) PrintHumanReadable() string { return string(s) }
func (s flakeSummary) SummaryKind() string        { return "Flake" }
func (s flakeSummary) PrintJSON() string          { return `{"type":"Flake"}` }

// additionalEvents is a test summary type that allows tests to add additional
// events to the summary
type additionalEvents struct {
	Events monitorapi.Intervals
}

func (s additionalEvents) PrintHumanReadable() string { return strings.Join(s.Events.Strings(), "\n") }
func (s additionalEvents) SummaryKind() string        { return "AdditionalEvents" }
func (s additionalEvents) PrintJSON() string {
	data, _ := monitorserialization.EventsIntervalsToJSON(s.Events)
	return string(data)
}

// TestData is passed to the invariant tests executed during the upgrade. The default UpgradeType
// is MasterUpgrade.
type TestData struct {
	UpgradeType    upgrades.UpgradeType
	UpgradeContext upgrades.UpgradeContext
}

// Run executes the provided fn in a test context, ensuring that invariants are preserved while the
// test is being executed. Description is used to populate the JUnit suite name, and testname is
// used to define the overall test that will be run.
func Run(f *framework.Framework, description, testname string, adapter TestData, invariants []upgrades.Test, fn func()) {
	testSuite := &junit.TestSuite{Name: description, Package: testname}
	test := &junit.TestCase{Name: testname, Classname: testname}
	testSuite.TestCases = append(testSuite.TestCases, test)
	cm := chaosmonkey.New(func() {
		start := time.Now()
		defer finalizeTest(start, test, testSuite, f)
		defer g.GinkgoRecover()
		fn()
	})
	runChaosmonkey(cm, adapter, invariants, testSuite)
}

func runChaosmonkey(
	cm *chaosmonkey.Chaosmonkey,
	testData TestData,
	tests []upgrades.Test,
	testSuite *junit.TestSuite,
) {
	testFrameworks := createTestFrameworks(tests)
	for _, t := range tests {
		displayName := t.Name()
		if dn, ok := t.(testWithDisplayName); ok {
			displayName = dn.DisplayName()
		}
		testCase := &junit.TestCase{
			Name:      displayName,
			Classname: "disruption_tests",
		}
		testSuite.TestCases = append(testSuite.TestCases, testCase)

		f, ok := testFrameworks[t.Name()]
		if !ok {
			panic(fmt.Sprintf("can't find test framework for %q", t.Name()))
		}
		cma := chaosMonkeyAdapter{
			TestData:        testData,
			framework:       f,
			test:            t,
			testReport:      testCase,
			testSuiteReport: testSuite,
		}
		cm.Register(cma.Test)
	}

	start := time.Now()
	defer func() {
		testSuite.Update()
		testSuite.Time = time.Since(start).Seconds()

		// if the test fails and all failures are described as "Flake", create a second
		// test case that is listed as success so the test is properly marked as flaky
		for _, testCase := range testSuite.TestCases {
			allFlakes := len(testCase.Failures) > 0 && len(testCase.Errors) == 0 && len(testCase.Skipped) == 0
			for _, failure := range testCase.Failures {
				if failure.Type == "Flake" {
					failure.Type = "Failure"
				} else {
					allFlakes = false
				}
			}
			if allFlakes {
				testSuite.TestCases = append(testSuite.TestCases, &junit.TestCase{
					Name:      testCase.Name,
					Classname: testCase.Classname,
					Time:      testCase.Time,
				})
			}
		}

		if framework.TestContext.ReportDir != "" {
			fname := filepath.Join(framework.TestContext.ReportDir, fmt.Sprintf("junit_%s_%d.xml", testSuite.Package, time.Now().Unix()))
			f, err := os.Create(fname)
			if err != nil {
				return
			}
			defer f.Close()
			xml.NewEncoder(f).Encode(testSuite)
		}
	}()
	cm.Do()
}

type chaosMonkeyAdapter struct {
	TestData

	test            upgrades.Test
	testReport      *junit.TestCase
	testSuiteReport *junit.TestSuite
	framework       *framework.Framework
}

func (cma *chaosMonkeyAdapter) Test(sem *chaosmonkey.Semaphore) {
	start := time.Now()
	var once sync.Once
	ready := func() {
		once.Do(func() {
			sem.Ready()
		})
	}
	defer finalizeTest(start, cma.testReport, cma.testSuiteReport, cma.framework)
	defer ready()
	if skippable, ok := cma.test.(upgrades.Skippable); ok && skippable.Skip(cma.UpgradeContext) {
		g.By("skipping test " + cma.test.Name())
		cma.testReport.Skipped = "skipping test " + cma.test.Name()
		return
	}
	cma.framework.BeforeEach()
	cma.test.Setup(cma.framework)
	defer cma.test.Teardown(cma.framework)
	ready()
	cma.test.Test(cma.framework, sem.StopCh, cma.UpgradeType)
}

func finalizeTest(start time.Time, tc *junit.TestCase, ts *junit.TestSuite, f *framework.Framework) {
	now := time.Now().UTC()
	tc.Time = now.Sub(start).Seconds()
	r := recover()

	// if the framework contains additional test results, add them to the parent suite or write them to disk
	for _, summary := range f.TestSummaries {
		if test, ok := summary.(additionalTest); ok {
			testCase := &junit.TestCase{
				Name: test.Name,
				Time: test.Duration.Seconds(),
			}
			if len(test.Failure) > 0 {
				testCase.Failures = append(testCase.Failures, &junit.Failure{
					Message: test.Failure,
					Value:   test.Failure,
				})
			}
			ts.TestCases = append(ts.TestCases, testCase)
			continue
		}

		filePath := filepath.Join(framework.TestContext.ReportDir, fmt.Sprintf("%s_%s_%s.json", summary.SummaryKind(), filesystemSafeName(tc.Name), now.Format(time.RFC3339)))
		if err := ioutil.WriteFile(filePath, []byte(summary.PrintJSON()), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error: Failed to write file %v with test data: %v\n", filePath, err)
		}
	}

	if r == nil {
		if f != nil {
			if message, ok := hasFrameworkFlake(f); ok {
				tc.Failures = append(tc.Failures, &junit.Failure{
					Type:    "Flake",
					Message: message,
					Value:   message,
				})
			}
		}
		return
	}
	framework.Logf("recover: %v", r)

	switch r := r.(type) {
	case ginkgowrapper.FailurePanic:
		tc.Failures = []*junit.Failure{
			{
				Message: r.Message,
				Type:    "Failure",
				Value:   fmt.Sprintf("%s\n\n%s", r.Message, r.FullStackTrace),
			},
		}
	case e2eskipper.SkipPanic:
		tc.Skipped = fmt.Sprintf("%s:%d %q", r.Filename, r.Line, r.Message)
	default:
		tc.Errors = []*junit.Error{
			{
				Message: fmt.Sprintf("%v", r),
				Type:    "Panic",
				Value:   fmt.Sprintf("%v\n\n%s", r, debug.Stack()),
			},
		}
	}
	// if we have a panic but it hasn't been recorded by ginkgo, panic now
	if !g.CurrentGinkgoTestDescription().Failed {
		framework.Logf("%q: panic: %v", tc.Name, r)
		func() {
			defer g.GinkgoRecover()
			panic(r)
		}()
	}
}

var (
	reFilesystemSafe      = regexp.MustCompile(`[^a-zA-Z1-9_]`)
	reFilesystemDuplicate = regexp.MustCompile(`_+`)
)

func filesystemSafeName(s string) string {
	s = reFilesystemSafe.ReplaceAllString(s, "_")
	return reFilesystemDuplicate.ReplaceAllString(s, "_")
}

// isGoModulePath returns true if the packagePath reported by reflection is within a
// module and given module path. When go mod is in use, module and modulePath are not
// contiguous as they were in older golang versions with vendoring, so naive contains
// tests fail.
//
// historically: ".../vendor/k8s.io/kubernetes/test/e2e"
// go.mod:       "k8s.io/kubernetes@0.18.4/test/e2e"
//
func isGoModulePath(packagePath, module, modulePath string) bool {
	return regexp.MustCompile(fmt.Sprintf(`\b%s(@[^/]*|)/%s\b`, regexp.QuoteMeta(module), regexp.QuoteMeta(modulePath))).MatchString(packagePath)
}

// TODO: accept a default framework
func createTestFrameworks(tests []upgrades.Test) map[string]*framework.Framework {
	nsFilter := regexp.MustCompile("[^[:word:]-]+") // match anything that's not a word character or hyphen
	testFrameworks := map[string]*framework.Framework{}
	for _, t := range tests {
		ns := nsFilter.ReplaceAllString(t.Name(), "-") // and replace with a single hyphen
		ns = strings.Trim(ns, "-")
		// identify tests that come from kube as strictly e2e tests so they get the correct semantics
		if isGoModulePath(reflect.ValueOf(t).Elem().Type().PkgPath(), "k8s.io/kubernetes", "test/e2e") {
			ns = "e2e-k8s-" + ns
		}
		testFrameworks[t.Name()] = &framework.Framework{
			BaseName:                 ns,
			AddonResourceConstraints: make(map[string]framework.ResourceConstraint),
			Options: framework.Options{
				ClientQPS:   20,
				ClientBurst: 50,
			},
			Timeouts: framework.NewTimeoutContextWithDefaults(),
		}
	}
	return testFrameworks
}

// ExpectNoDisruption fails if the sum of the duration of all events exceeds tolerate as a fraction ([0-1]) of total, reports a
// disruption flake if any disruption occurs, and uses reason to prefix the message. I.e. tolerate 0.1 of 10m total will fail
// if the sum of the intervals is greater than 1m, or report a flake if any interval is found.
func ExpectNoDisruption(f *framework.Framework, tolerate float64, total time.Duration, events monitorapi.Intervals, reason string) {
	FrameworkEventIntervals(f, events)
	duration := events.Duration(0, 1*time.Second)
	describe := events.Strings()
	if percent := float64(duration) / float64(total); percent > tolerate {
		framework.Failf("%s for at least %s of %s (%0.0f%%, but only %0.0f%% is tolerated):\n\n%s", reason, duration.Truncate(time.Second), total.Truncate(time.Second), percent*100, tolerate*100, strings.Join(describe, "\n"))
	} else if duration > 0 {
		FrameworkFlakef(f, "%s for at least %s of %s (%0.0f%%), this is currently sufficient to pass the"+
			" test/job but not considered completely correct.\nTolerating up to %0.0f%% disruption:\n\n%s",
			reason, duration.Truncate(time.Second), total.Truncate(time.Second), percent*100, tolerate*100,
			strings.Join(describe, "\n"))
	}
}

// FrameworkFlakef records a flake on the current framework.
func FrameworkFlakef(f *framework.Framework, format string, options ...interface{}) {
	framework.Logf(format, options...)
	f.TestSummaries = append(f.TestSummaries, flakeSummary(fmt.Sprintf(format, options...)))
}

// FrameworkFlakef records a flake on the current framework.
func FrameworkEventIntervals(f *framework.Framework, events monitorapi.Intervals) {
	f.TestSummaries = append(f.TestSummaries, additionalEvents{Events: events})
}

// hasFrameworkFlake returns true if the framework recorded a flake message generated by
// Flakef during the test run.
func hasFrameworkFlake(f *framework.Framework) (string, bool) {
	for _, summary := range f.TestSummaries {
		s, ok := summary.(flakeSummary)
		if !ok {
			continue
		}
		return string(s), true
	}
	return "", false
}

// RecordJUnit will capture the result of invoking fn as either a passing or failing JUnit test
// that will be recorded alongside the current test with name. These methods only work in the
// context of a disruption test suite today and will not be reported as JUnit failures when
// used within normal ginkgo suties.
func RecordJUnit(f *framework.Framework, name string, fn func() error) error {
	start := time.Now()
	err := fn()
	duration := time.Now().Sub(start)
	var failure string
	if err != nil {
		failure = err.Error()
	}
	f.TestSummaries = append(f.TestSummaries, additionalTest{
		Name:     name,
		Duration: duration,
		Failure:  failure,
	})
	return err
}

// RecordJUnitResult will output a junit result within a disruption test with the given name,
// duration, and failure string. If the failure string is set, the test is considered to have
// failed, otherwise the test is considered to have passed. These methods only work in the
// context of a disruption test suite today and will not be reported as JUnit failures when
// used within normal ginkgo suties.
func RecordJUnitResult(f *framework.Framework, name string, duration time.Duration, failure string) {
	f.TestSummaries = append(f.TestSummaries, additionalTest{
		Name:     name,
		Duration: duration,
		Failure:  failure,
	})
}
