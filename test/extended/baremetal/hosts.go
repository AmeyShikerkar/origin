package baremetal

import (
	"context"
	"fmt"
	"strings"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	configv1 "github.com/openshift/api/config/v1"
	exutil "github.com/openshift/origin/test/extended/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

func skipIfNotBaremetal(oc *exutil.CLI) {
	g.By("checking platform type")

	infra, err := oc.AdminConfigClient().ConfigV1().Infrastructures().Get(context.Background(), "cluster", metav1.GetOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())

	if infra.Status.PlatformStatus.Type != configv1.BareMetalPlatformType {
		e2eskipper.Skipf("No baremetal platform detected")
	}
}

func baremetalClient(dc dynamic.Interface) dynamic.ResourceInterface {
	baremetalClient := dc.Resource(schema.GroupVersionResource{Group: "metal3.io", Resource: "baremetalhosts", Version: "v1alpha1"})
	return baremetalClient.Namespace("openshift-machine-api")
}

func hostfirmwaresettingsClient(dc dynamic.Interface) dynamic.ResourceInterface {
	hfsClient := dc.Resource(schema.GroupVersionResource{Group: "metal3.io", Resource: "hostfirmwaresettings", Version: "v1alpha1"})
	return hfsClient.Namespace("openshift-machine-api")
}

type FieldGetterFunc func(obj map[string]interface{}, fields ...string) (interface{}, bool, error)

func expectField(host unstructured.Unstructured, resource string, nestedField string, fieldGetter FieldGetterFunc) o.Assertion {
	fields := strings.Split(nestedField, ".")

	value, found, err := fieldGetter(host.Object, fields...)
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(found).To(o.BeTrue(), fmt.Sprintf("`%s` field `%s` not found", resource, nestedField))
	return o.Expect(value)
}

func expectStringField(host unstructured.Unstructured, resource string, nestedField string) o.Assertion {
	return expectField(host, resource, nestedField, func(obj map[string]interface{}, fields ...string) (interface{}, bool, error) {
		return unstructured.NestedString(host.Object, fields...)
	})
}

func expectBoolField(host unstructured.Unstructured, resource string, nestedField string) o.Assertion {
	return expectField(host, resource, nestedField, func(obj map[string]interface{}, fields ...string) (interface{}, bool, error) {
		return unstructured.NestedBool(host.Object, fields...)
	})
}

func expectStringMapField(host unstructured.Unstructured, resource string, nestedField string) o.Assertion {
	return expectField(host, resource, nestedField, func(obj map[string]interface{}, fields ...string) (interface{}, bool, error) {
		return unstructured.NestedStringMap(host.Object, fields...)
	})
}

func expectSliceField(host unstructured.Unstructured, resource string, nestedField string) o.Assertion {
	return expectField(host, resource, nestedField, func(obj map[string]interface{}, fields ...string) (interface{}, bool, error) {
		return unstructured.NestedSlice(host.Object, fields...)
	})
}

// Conditions are stored as a slice of maps, check that the type has the correct status
func checkConditionStatus(hfs unstructured.Unstructured, condType string, condStatus string) {

	conditions, _, err := unstructured.NestedSlice(hfs.Object, "status", "conditions")
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(conditions).ToNot(o.BeEmpty())

	for _, c := range conditions {
		condition, ok := c.(map[string]interface{})
		o.Expect(ok).To(o.BeTrue())

		t, ok := condition["type"]
		o.Expect(ok).To(o.BeTrue())
		if t == condType {
			s, ok := condition["status"]
			o.Expect(ok).To(o.BeTrue())
			o.Expect(s).To(o.Equal(condStatus))
		}
	}
}

func getField(host unstructured.Unstructured, resource string, nestedField string, fieldGetter FieldGetterFunc) string {
	fields := strings.Split(nestedField, ".")

	value, found, err := fieldGetter(host.Object, fields...)
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(found).To(o.BeTrue(), fmt.Sprintf("`%s` field `%s` not found", resource, nestedField))
	return value.(string)
}

func getStringField(host unstructured.Unstructured, resource string, nestedField string) string {
	return getField(host, resource, nestedField, func(obj map[string]interface{}, fields ...string) (interface{}, bool, error) {
		return unstructured.NestedFieldNoCopy(host.Object, fields...)
	})
}

var _ = g.Describe("[sig-installer][Feature:baremetal] Baremetal platform should", func() {
	defer g.GinkgoRecover()

	oc := exutil.NewCLI("baremetal")

	g.It("have a metal3 deployment", func() {
		skipIfNotBaremetal(oc)

		c, err := e2e.LoadClientset()
		o.Expect(err).ToNot(o.HaveOccurred())

		metal3, err := c.AppsV1().Deployments("openshift-machine-api").Get(context.Background(), "metal3", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(metal3.Status.AvailableReplicas).To(o.BeEquivalentTo(1))

		o.Expect(metal3.Annotations).Should(o.HaveKey("baremetal.openshift.io/owned"))
		o.Expect(metal3.Labels).Should(o.HaveKeyWithValue("baremetal.openshift.io/cluster-baremetal-operator", "metal3-state"))
	})

	g.It("have baremetalhost resources", func() {
		skipIfNotBaremetal(oc)

		dc := oc.AdminDynamicClient()
		bmc := baremetalClient(dc)

		hosts, err := bmc.List(context.Background(), v1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(hosts.Items).ToNot(o.BeEmpty())

		for _, h := range hosts.Items {
			expectStringField(h, "baremetalhost", "status.provisioning.state").To(o.Or(o.BeEquivalentTo("provisioned"), o.BeEquivalentTo("externally provisioned")))
			state := getStringField(h, "baremetalhost", "status.provisioning.state")
			// When testing with CoreOS preprovisioning images, masters will faild to be adopted properly due to BZ 2032573
			// Remove this check when fix for BZ 2032573 merges
			if state != "externally provisioned" {
				hostName := getStringField(h, "baremetalhost", "metadata.name")
				g.By(fmt.Sprintf("check that baremetalhost %s operationalStatus is OK", hostName))
				expectStringField(h, "baremetalhost", "status.operationalStatus").To(o.BeEquivalentTo("OK"))
			}
			expectBoolField(h, "baremetalhost", "spec.online").To(o.BeTrue())
		}
	})

	g.It("have hostfirmwaresetting resources", func() {
		skipIfNotBaremetal(oc)

		dc := oc.AdminDynamicClient()

		bmc := baremetalClient(dc)
		hosts, err := bmc.List(context.Background(), v1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(hosts.Items).ToNot(o.BeEmpty())

		hfsClient := hostfirmwaresettingsClient(dc)

		for _, h := range hosts.Items {
			hostName := getStringField(h, "baremetalhost", "metadata.name")

			g.By(fmt.Sprintf("check that baremetalhost %s has a corresponding hostfirmwaresettings", hostName))
			hfs, err := hfsClient.Get(context.Background(), hostName, v1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(hfs).NotTo(o.Equal(nil))

			// Reenable this when fix to prevent settings with 0 entries is in BMO
			// g.By("check that hostfirmwaresettings settings have been populated")
			// expectStringMapField(*hfs, "hostfirmwaresettings", "status.settings").ToNot(o.BeEmpty())

			g.By("check that hostfirmwaresettings conditions show resource is valid")
			checkConditionStatus(*hfs, "Valid", "True")

			g.By("check that hostfirmwaresettings reference a schema")
			refName := getStringField(*hfs, "hostfirmwaresettings", "status.schema.name")
			refNS := getStringField(*hfs, "hostfirmwaresettings", "status.schema.namespace")

			schemaClient := dc.Resource(schema.GroupVersionResource{Group: "metal3.io", Resource: "firmwareschemas", Version: "v1alpha1"}).Namespace(refNS)
			schema, err := schemaClient.Get(context.Background(), refName, v1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(schema).NotTo(o.Equal(nil))
		}
	})

	g.It("not allow updating BootMacAddress", func() {
		skipIfNotBaremetal(oc)

		dc := oc.AdminDynamicClient()
		bmc := baremetalClient(dc)

		hosts, err := bmc.List(context.Background(), v1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(hosts.Items).ToNot(o.BeEmpty())

		host := hosts.Items[0]
		expectStringField(host, "baremetalhost", "spec.bootMACAddress").ShouldNot(o.BeNil())
		// Already verified that bootMACAddress exists
		bootMACAddress, _, _ := unstructured.NestedString(host.Object, "spec", "bootMACAddress")
		testMACAddress := "11:11:11:11:11:11"

		g.By("updating bootMACAddress which is not allowed")
		err = unstructured.SetNestedField(host.Object, testMACAddress, "spec", "bootMACAddress")
		o.Expect(err).NotTo(o.HaveOccurred())
		_, err = bmc.Update(context.Background(), &host, v1.UpdateOptions{})
		o.Expect(err).To(o.HaveOccurred())
		o.Expect(err.Error()).To(o.ContainSubstring("bootMACAddress can not be changed once it is set"))

		g.By("verify bootMACAddress is not updated")
		h, err := bmc.Get(context.Background(), host.GetName(), v1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		check, _, _ := unstructured.NestedString(h.Object, "spec", "bootMACAddress")
		o.Expect(check).To(o.Equal(bootMACAddress))
	})
})
