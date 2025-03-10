package service

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/openshift/origin/pkg/monitor/monitorapi"

	"github.com/onsi/ginkgo"
	configv1 "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/origin/pkg/monitor"
	"github.com/openshift/origin/test/extended/util/disruption"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enetwork "k8s.io/kubernetes/test/e2e/framework/network"
	"k8s.io/kubernetes/test/e2e/framework/service"
	"k8s.io/kubernetes/test/e2e/upgrades"
)

// UpgradeTest tests that a service is available before, during, and
// after a cluster upgrade.
type UpgradeTest struct {
	jig        *service.TestJig
	tcpService *v1.Service

	unsupportedPlatform bool
}

func (UpgradeTest) Name() string { return "k8s-service-lb-available" }
func (UpgradeTest) DisplayName() string {
	return "[sig-network-edge] Application behind service load balancer with PDB is not disrupted"
}

func shouldTestPDBs() bool { return true }

// Setup creates a service with a load balancer and makes sure it's reachable.
func (t *UpgradeTest) Setup(f *framework.Framework) {
	configClient, err := configclient.NewForConfig(f.ClientConfig())
	framework.ExpectNoError(err)
	infra, err := configClient.ConfigV1().Infrastructures().Get(context.Background(), "cluster", metav1.GetOptions{})
	framework.ExpectNoError(err)
	// ovirt does not support service type loadbalancer because it doesn't program a cloud.
	if infra.Status.PlatformStatus.Type == configv1.OvirtPlatformType || infra.Status.PlatformStatus.Type == configv1.KubevirtPlatformType || infra.Status.PlatformStatus.Type == configv1.LibvirtPlatformType || infra.Status.PlatformStatus.Type == configv1.VSpherePlatformType || infra.Status.PlatformStatus.Type == configv1.BareMetalPlatformType {
		t.unsupportedPlatform = true
	}
	// single node clusters are not supported because the replication controller has 2 replicas with anti-affinity for running on the same node.
	if infra.Status.ControlPlaneTopology == configv1.SingleReplicaTopologyMode {
		t.unsupportedPlatform = true
	}
	if t.unsupportedPlatform {
		return
	}

	serviceName := "service-test"
	jig := service.NewTestJig(f.ClientSet, f.Namespace.Name, serviceName)

	ns := f.Namespace
	cs := f.ClientSet

	ginkgo.By("creating a TCP service " + serviceName + " with type=LoadBalancer in namespace " + ns.Name)
	tcpService, err := jig.CreateTCPService(func(s *v1.Service) {
		s.Spec.Type = v1.ServiceTypeLoadBalancer
		// ServiceExternalTrafficPolicyTypeCluster performs during disruption, Local does not
		s.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
		if s.Annotations == nil {
			s.Annotations = make(map[string]string)
		}
		// We tune the LB checks to match the longest intervals available so that interactions between
		// upgrading components and the service are more obvious.
		// - AWS allows configuration, default is 70s (6 failed with 10s interval in 1.17) set to match GCP
		s.Annotations["service.beta.kubernetes.io/aws-load-balancer-healthcheck-interval"] = "8"
		s.Annotations["service.beta.kubernetes.io/aws-load-balancer-healthcheck-unhealthy-threshold"] = "3"
		s.Annotations["service.beta.kubernetes.io/aws-load-balancer-healthcheck-healthy-threshold"] = "2"
		// - Azure is hardcoded to 15s (2 failed with 5s interval in 1.17) and is sufficient
		// - GCP has a non-configurable interval of 32s (3 failed health checks with 8s interval in 1.17)
		//   - thus pods need to stay up for > 32s, so pod shutdown period will will be 45s
	})
	framework.ExpectNoError(err)
	tcpService, err = jig.WaitForLoadBalancer(service.GetServiceLoadBalancerCreationTimeout(cs))
	framework.ExpectNoError(err)

	// Get info to hit it with
	tcpIngressIP := service.GetIngressPoint(&tcpService.Status.LoadBalancer.Ingress[0])
	svcPort := int(tcpService.Spec.Ports[0].Port)

	ginkgo.By("creating RC to be part of service " + serviceName)
	rc, err := jig.Run(func(rc *v1.ReplicationController) {
		// ensure the pod waits long enough during update for the LB to see the newly ready pod, which
		// must be longer than the worst load balancer above (GCP at 32s)
		rc.Spec.MinReadySeconds = 33
		// ensure the pod waits long enough for most LBs to take it out of rotation, which has to be
		// longer than the LB failed health check duration + 1 cycle
		rc.Spec.Template.Spec.Containers[0].Lifecycle = &v1.Lifecycle{
			PreStop: &v1.Handler{
				Exec: &v1.ExecAction{Command: []string{"sleep", "45"}},
			},
		}
		// ensure the pod is not forcibly deleted at 30s, but waits longer than the graceful sleep
		minute := int64(60)
		rc.Spec.Template.Spec.TerminationGracePeriodSeconds = &minute

		jig.AddRCAntiAffinity(rc)
	})
	framework.ExpectNoError(err)

	if shouldTestPDBs() {
		ginkgo.By("creating a PodDisruptionBudget to cover the ReplicationController")
		_, err = jig.CreatePDB(rc)
		framework.ExpectNoError(err)
	}

	// Hit it once before considering ourselves ready
	ginkgo.By("hitting pods through the service's LoadBalancer")
	timeout := 10 * time.Minute
	// require thirty seconds of passing requests to continue (in case the SLB becomes available and then degrades)
	TestReachableHTTPWithMinSuccessCount(tcpIngressIP, svcPort, 30, timeout)

	t.jig = jig
	t.tcpService = tcpService
}

// Test runs a connectivity check to the service.
func (t *UpgradeTest) Test(f *framework.Framework, done <-chan struct{}, upgrade upgrades.UpgradeType) {
	if t.unsupportedPlatform {
		return
	}

	client, err := framework.LoadClientset()
	framework.ExpectNoError(err)

	stopCh := make(chan struct{})
	defer close(stopCh)
	newBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: client.EventsV1()})
	r := newBroadcaster.NewRecorder(scheme.Scheme, "openshift.io/upgrade-test-service")
	newBroadcaster.StartRecordingToSink(stopCh)

	ginkgo.By("continuously hitting pods through the service's LoadBalancer")

	ctx, cancel := context.WithCancel(context.Background())
	m := monitor.NewMonitorWithInterval(1 * time.Second)
	err = startEndpointMonitoring(ctx, m, t.tcpService, r)
	framework.ExpectNoError(err, "unable to monitor API")

	start := time.Now()
	m.StartSampling(ctx)

	// wait to ensure API is still up after the test ends
	<-done
	ginkgo.By("waiting for any post disruption failures")
	time.Sleep(15 * time.Second)
	cancel()
	end := time.Now()

	disruption.ExpectNoDisruption(f, 0.02, end.Sub(start), m.Intervals(time.Time{}, time.Time{}), "Service was unreachable during disruption")

	// verify finalizer behavior
	defer func() {
		ginkgo.By("Check that service can be deleted with finalizer")
		service.WaitForServiceDeletedWithFinalizer(t.jig.Client, t.tcpService.Namespace, t.tcpService.Name)
	}()
	ginkgo.By("Check that finalizer is present on loadBalancer type service")
	service.WaitForServiceUpdatedWithFinalizer(t.jig.Client, t.tcpService.Namespace, t.tcpService.Name, true)
}

// Teardown cleans up any remaining resources.
func (t *UpgradeTest) Teardown(f *framework.Framework) {
	// rely on the namespace deletion to clean up everything
}

func startEndpointMonitoring(ctx context.Context, m *monitor.Monitor, svc *v1.Service, r events.EventRecorder) error {
	tcpIngressIP := service.GetIngressPoint(&svc.Status.LoadBalancer.Ingress[0])
	svcPort := int(svc.Spec.Ports[0].Port)
	url := fmt.Sprintf("http://%s/echo?msg=Hello", net.JoinHostPort(tcpIngressIP, strconv.Itoa(svcPort)))
	// this client reuses connections and detects abrupt breaks
	continuousClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Dial: (&net.Dialer{
				Timeout: 15 * time.Second,
			}).Dial,
			TLSHandshakeTimeout: 15 * time.Second,
		},
	}

	reusedConnectionLocator := monitor.LocateDisruptionCheck("service-loadbalancer-with-pdb", monitor.ReusedConnectionType)
	newConnectionLocator := monitor.LocateDisruptionCheck("service-loadbalancer-with-pdb", monitor.NewConnectionType)
	go monitor.NewSampler(m, time.Second, func(previous bool) (condition *monitorapi.Condition, next bool) {
		resp, err := continuousClient.Get(url)
		if err == nil {
			defer resp.Body.Close()
			body, err := ioutil.ReadAll(resp.Body)
			if err == nil && !bytes.Contains(body, []byte("Hello")) {
				err = fmt.Errorf("service returned success but did not contain the correct body contents: %q", string(body))
			}
		}
		switch {
		case err == nil && !previous:
			condition = &monitorapi.Condition{
				Level:   monitorapi.Info,
				Locator: reusedConnectionLocator,
				Message: monitor.DisruptionEndedMessage(reusedConnectionLocator, monitor.ReusedConnectionType),
			}
		case err != nil && previous:
			framework.Logf("Service %s is unreachable on reused connections: %v", svc.Name, err)
			r.Eventf(&v1.ObjectReference{Kind: "Service", Namespace: "kube-system", Name: "service-upgrade-test"}, nil, v1.EventTypeWarning, "Unreachable", "detected", "on reused connections")
			condition = &monitorapi.Condition{
				Level:   monitorapi.Error,
				Locator: reusedConnectionLocator,
				Message: monitor.DisruptionBeganMessage(reusedConnectionLocator, monitor.ReusedConnectionType, err),
			}
		case err != nil:
			framework.Logf("Service %s is unreachable on reused connections: %v", svc.Name, err)
		}
		return condition, err == nil
	}).WhenFailing(ctx, &monitorapi.Condition{
		Level:   monitorapi.Error,
		Locator: reusedConnectionLocator,
		Message: monitor.DisruptionContinuingMessage(reusedConnectionLocator, monitor.ReusedConnectionType, fmt.Errorf("missing error in the code")),
	})

	// this client creates fresh connections and detects failure to establish connections
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Dial: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: -1,
			}).Dial,
			TLSHandshakeTimeout: 15 * time.Second,
			IdleConnTimeout:     15 * time.Second,
			DisableKeepAlives:   true,
		},
	}

	go monitor.NewSampler(m, time.Second, func(previous bool) (condition *monitorapi.Condition, next bool) {
		resp, err := client.Get(url)
		if err == nil {
			defer resp.Body.Close()
			body, err := ioutil.ReadAll(resp.Body)
			if err == nil && !bytes.Contains(body, []byte("Hello")) {
				err = fmt.Errorf("service returned success but did not contain the correct body contents: %q", string(body))
			}
		}
		switch {
		case err == nil && !previous:
			condition = &monitorapi.Condition{
				Level:   monitorapi.Info,
				Locator: newConnectionLocator,
				Message: monitor.DisruptionEndedMessage(newConnectionLocator, monitor.NewConnectionType),
			}
		case err != nil && previous:
			framework.Logf("Service %s is unreachable on new connections: %v", svc.Name, err)
			r.Eventf(&v1.ObjectReference{Kind: "Service", Namespace: "kube-system", Name: "service-upgrade-test"}, nil, v1.EventTypeWarning, "Unreachable", "detected", "on new connections")
			condition = &monitorapi.Condition{
				Level:   monitorapi.Error,
				Locator: newConnectionLocator,
				Message: monitor.DisruptionBeganMessage(newConnectionLocator, monitor.NewConnectionType, err),
			}
		case err != nil:
			framework.Logf("Service %s is unreachable on new connections: %v", svc.Name, err)
		}
		return condition, err == nil
	}).WhenFailing(ctx, &monitorapi.Condition{
		Level:   monitorapi.Error,
		Locator: newConnectionLocator,
		Message: monitor.DisruptionContinuingMessage(newConnectionLocator, monitor.NewConnectionType, fmt.Errorf("missing error in the code")),
	})

	return nil
}

func locateService(svc *v1.Service) string {
	return fmt.Sprintf("ns/%s svc/%s", svc.Namespace, svc.Name)
}

// TestReachableHTTPWithMinSuccessCount tests that the given host serves HTTP on the given port for a minimum of successCount number of
// counts at a given interval. If the service reachability fails, the counter gets reset
func TestReachableHTTPWithMinSuccessCount(host string, port int, successCount int, timeout time.Duration) {
	consecutiveSuccessCnt := 0
	err := wait.PollImmediate(framework.Poll, timeout, func() (bool, error) {
		result := e2enetwork.PokeHTTP(host, port, "/echo?msg=hello",
			&e2enetwork.HTTPPokeParams{
				BodyContains:   "hello",
				RetriableCodes: []int{},
			})
		if result.Status == e2enetwork.HTTPSuccess {
			consecutiveSuccessCnt++
			return consecutiveSuccessCnt >= successCount, nil
		}
		consecutiveSuccessCnt = 0
		return false, nil // caller can retry
	})
	framework.ExpectNoError(err)
}
