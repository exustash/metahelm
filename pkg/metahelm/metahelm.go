package metahelm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dollarshaveclub/metahelm/pkg/dag"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/typed/extensions/v1beta1"
	"k8s.io/helm/pkg/helm"
	rls "k8s.io/helm/pkg/proto/hapi/services"
	deploymentutil "k8s.io/kubernetes/pkg/controller/deployment/util"
)

// K8sClient describes an object that functions as a Kubernetes client
type K8sClient interface {
	ExtensionsV1beta1() v1beta1.ExtensionsV1beta1Interface
}

// HelmClient describes an object that functions as a Helm client
type HelmClient interface {
	InstallRelease(chstr, ns string, opts ...helm.InstallOption) (*rls.InstallReleaseResponse, error)
}

// Manager is an object that manages installation of chart graphs
type Manager struct {
	K8c K8sClient
	HC  HelmClient
}

type options struct {
	k8sNamespace    string
	tillerNamespace string
	installCallback InstallCallback
}

type InstallOption func(*options)

// WithK8sNamespace specifies the kubernetes namespace to install a chart graph into. DefaultK8sNamespace is used otherwise.
func WithK8sNamespace(ns string) InstallOption {
	return func(op *options) {
		op.k8sNamespace = ns
	}
}

// WithTillerNamespace specifies the namespace where the Tiller service can be found
func WithTillerNamespace(tns string) InstallOption {
	return func(op *options) {
		op.tillerNamespace = tns
	}
}

// WithInstallCallback specifies a callback function that will be invoked immediately prior to each chart installation
func WithInstallCallback(cb InstallCallback) InstallOption {
	return func(op *options) {
		op.installCallback = cb
	}
}

// CallbackAction indicates the decision made by the callback
type InstallCallbackAction int

const (
	// Continue indicates the installation should proceed immediately
	Continue InstallCallbackAction = iota
	// Wait means the install should not happen right now but should be retried at some point in the future. The callback will be invoked again on the retry.
	Wait
	// Abort means the installation should not be attempted
	Abort
)

// InstallCallback is a function that decides whether to proceed with an individual chart installation
// This will be called concurrently from multiple goroutines, so make sure everything is threadsafe
type InstallCallback func(Chart) InstallCallbackAction

// ReleaseMap is a map of chart title to installed release name
type ReleaseMap map[string]string

// release names
type lockingReleases struct {
	sync.Mutex
	rmap ReleaseMap
}

// DefaultK8sNamespace is the k8s namespace to install a chart graph into if not specified
const DefaultK8sNamespace = "default"
const retryDelay = 10 * time.Second

// Install installs charts in order according to dependencies and returns the names of the releases, or error
func (m *Manager) Install(ctx context.Context, charts []Chart, opts ...InstallOption) (ReleaseMap, error) {
	ops := &options{}
	for _, opt := range opts {
		opt(ops)
	}
	if len(charts) == 0 {
		return nil, errors.New("no charts were supplied")
	}
	if ops.k8sNamespace == "" {
		ops.k8sNamespace = DefaultK8sNamespace
	}
	cmap := map[string]*Chart{}
	objs := []dag.GraphObject{}
	for i := range charts {
		if charts[i].WaitTimeout == 0 {
			charts[i].WaitTimeout = DefaultDeploymentTimeout
		}
		if charts[i].Location == "" {
			return nil, fmt.Errorf("empty location for chart: %v (offset %v)", charts[i].Title, i)
		}
		switch charts[i].DeploymentHealthIndication {
		case IgnorePodHealth:
		case AllPodsHealthy:
		case AtLeastOnePodHealthy:
		default:
			return nil, fmt.Errorf("unknown value for DeploymentHealthIndication: %v", charts[i].DeploymentHealthIndication)
		}
		cmap[charts[i].Name()] = &charts[i]
		objs = append(objs, &charts[i])
	}
	og := dag.ObjectGraph{}
	if err := og.Build(objs); err != nil {
		return nil, errors.Wrap(err, "error building graph")
	}
	rn := lockingReleases{rmap: make(map[string]string)}
	af := func(obj dag.GraphObject) error {
		for {
			if ops.installCallback == nil {
				break
			}
			v := ops.installCallback(*cmap[obj.Name()])
			switch v {
			case Continue:
				break
			case Wait:
				time.Sleep(retryDelay)
			case Abort:
				return errors.New("callback requested abort")
			default:
				return fmt.Errorf("unknown callback result: %v", v)
			}
		}
		c := cmap[obj.Name()]
		resp, err := m.HC.InstallRelease(c.Location, ops.k8sNamespace, helm.ValueOverrides(c.ValueOverrides))
		if err != nil {
			return errors.Wrap(err, "error installing chart")
		}
		rn.Lock()
		rn.rmap[c.Title] = resp.Release.Name
		rn.Unlock()
		return m.waitForChart(ctx, c, ops.k8sNamespace)
	}
	if err := og.Walk(ctx, af); err != nil {
		return nil, errors.Wrap(err, "error running installs")
	}
	return rn.rmap, nil
}

const chartWaitPollInterval = 10 * time.Second

func (m *Manager) waitForChart(ctx context.Context, c *Chart, ns string) error {
	if c.DeploymentHealthIndication == IgnorePodHealth {
		return nil
	}
	return wait.Poll(chartWaitPollInterval, c.WaitTimeout, func() (bool, error) {
		d, err := m.K8c.ExtensionsV1beta1().Deployments(ns).Get(c.WaitUntilDeployment, metav1.GetOptions{})
		if err != nil || d.Spec.Replicas == nil {
			return false, errors.Wrap(err, "error getting deployment")
		}

		rs, err := deploymentutil.GetNewReplicaSet(d, m.K8c.ExtensionsV1beta1())
		if err != nil {
			return false, errors.Wrap(err, "error getting new replica set")
		}

		if rs != nil {
			needed := 1
			if c.DeploymentHealthIndication == AllPodsHealthy {
				needed = int(*d.Spec.Replicas)
			}
			return int(rs.Status.ReadyReplicas) >= needed, nil
		}
		return false, nil
	})
}
