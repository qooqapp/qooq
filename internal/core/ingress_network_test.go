package core

import (
	"context"
	"reflect"
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

// Users never share the ingress network: a
// new component is created on its own network only, and it is the proxy that
// joins that network — on the shared one, any user's app could reach any
// other's database by its qualified name.
func TestTheProxyJoinsTheComponentNotTheOtherWayRound(t *testing.T) {
	rt, st := newFakeRuntime(), newFakeStore()
	svc := newTestService(rt, st)
	svc.SetIngressNetwork("qooq-ingress")
	ctx := context.Background()

	comp, err := svc.Deploy(ctx, ComponentSpec{Name: "web", Namespace: "acme--prod", Image: "nginx", Network: "qooq-acme--prod-web",
		Ports: []runtime.PortMapping{{ContainerPort: 80}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range rt.callArgs("ConnectNetwork") {
		if a == "qooq-ingress" {
			t.Fatal("a new component joined the shared ingress network")
		}
	}

	if err := svc.ConnectProxy(ctx, "proxy-cid", comp); err != nil {
		t.Fatal(err)
	}
	if got := rt.callArgs("ConnectNetworkAs"); !reflect.DeepEqual(got[len(got)-1:], []string{"proxy-cid@qooq-acme--prod-web"}) {
		t.Errorf("connects = %v, want the proxy on the component's network", got)
	}
	if err := svc.LeaveIngressNetwork(ctx, comp); err != nil {
		t.Fatal(err)
	}
	if got := rt.callArgs("DisconnectNetwork"); !reflect.DeepEqual(got, []string{comp.Status.ContainerID + "@qooq-ingress"}) {
		t.Errorf("disconnects = %v, want the component off the shared network", got)
	}
}

// A component with no network of its own is on the default bridge, where
// names do not resolve; it alone still joins the shared network, and is not
// taken off it.
func TestAComponentWithoutANetworkStillJoinsTheShared(t *testing.T) {
	rt, st := newFakeRuntime(), newFakeStore()
	svc := newTestService(rt, st)
	svc.SetIngressNetwork("qooq-ingress")
	ctx := context.Background()
	comp, err := svc.Deploy(ctx, ComponentSpec{Name: "old", Image: "nginx", Ports: []runtime.PortMapping{{ContainerPort: 80}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ConnectProxy(ctx, "proxy-cid", comp); err != nil {
		t.Fatal(err)
	}
	if got := rt.callArgs("ConnectNetworkAs"); len(got) == 0 || got[len(got)-1] != comp.Status.ContainerID+"@qooq-ingress" {
		t.Errorf("connects = %v, want the component itself on the shared network", got)
	}
	if err := svc.LeaveIngressNetwork(ctx, comp); err != nil {
		t.Fatal(err)
	}
	if n := rt.callCount("DisconnectNetwork"); n != 0 {
		t.Errorf("took a network-less component off its only way in (%d disconnects)", n)
	}
}

// -publish-ip binds what a spec leaves open, and is recorded on the component.
func TestPublishIPFillsOnlyWhatIsUnset(t *testing.T) {
	rt, st := newFakeRuntime(), newFakeStore()
	svc := newTestService(rt, st)
	if err := svc.SetPublishIP("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetPublishIP("not-an-ip"); err == nil {
		t.Error("a bad address was accepted")
	}
	comp, err := svc.Deploy(context.Background(), ComponentSpec{Name: "web", Image: "nginx", Ports: []runtime.PortMapping{
		{ContainerPort: 80, HostPort: 19000}, {ContainerPort: 81, HostPort: 19001, HostIP: "0.0.0.0"}, {ContainerPort: 82}}})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{comp.Spec.Ports[0].HostIP, comp.Spec.Ports[1].HostIP, comp.Spec.Ports[2].HostIP}
	if !reflect.DeepEqual(got, []string{"127.0.0.1", "0.0.0.0", ""}) {
		t.Errorf("host IPs = %v", got)
	}
}
