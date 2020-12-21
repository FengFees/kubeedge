package defaults

import (
	"context"
	"fmt"
	ingress "github.com/kubeedge/kubeedge/edge/pkg/edgegateway/Ingress"
	"github.com/kubeedge/kubeedge/edge/pkg/edgegateway/Ingress/controller"
	"github.com/kubeedge/kubeedge/edge/pkg/edgegateway/Ingress/task"
	"github.com/pkg/errors"
	"io/ioutil"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// UpdateInterval defines the time interval, in seconds, in
// which the status should check if an update is required.
var UpdateInterval = 60

// StatusPort port used by NGINX for the status server
var StatusPort = 10246

// Syncer ...
type Syncer interface {
	Run(chan struct{})

	Shutdown()
}

// Config ...
type Config struct {
	Client kubernetes.Interface

	PublishService string

	PublishStatusAddress string

	UpdateStatusOnShutdown bool

	UseNodeInternalIP bool

	IngressLister ingressLister
}

type ingressLister interface {
	// ListIngresses returns the list of Ingresses
	ListIngresses() []*ingress.Ingress
}

// statusSync keeps the status IP in each Ingress rule updated executing a periodic check
// in all the defined rules. To simplify the process leader election is used so the update
// is executed only in one node (Ingress controllers can be scaled to more than one)
// If the controller is running with the flag --publish-service (with a valid service)
// the IP address behind the service is used, if it is running with the flag
// --publish-status-address, the address specified in the flag is used, if neither of the
// two flags are set, the source is the IP/s of the node/s
type statusSync struct {
	Config

	// workqueue used to keep in sync the status IP/s
	// in the Ingress rules
	syncQueue *task.Queue
}

// Start starts the loop to keep the status in sync
func (s statusSync) Run(stopCh chan struct{}) {
	go s.syncQueue.Run(time.Second, stopCh)

	// trigger initial sync
	s.syncQueue.EnqueueTask(task.GetDummyObject("sync status"))

	// when this instance is the leader we need to enqueue
	// an item to trigger the update of the Ingress status.
	wait.PollUntil(time.Duration(UpdateInterval)*time.Second, func() (bool, error) {
		s.syncQueue.EnqueueTask(task.GetDummyObject("sync status"))
		return false, nil
	}, stopCh)
}

// Shutdown stops the sync. In case the instance is the leader it will remove the current IP
// if there is no other instances running.
func (s statusSync) Shutdown() {
	go s.syncQueue.Shutdown()

	if !s.UpdateStatusOnShutdown {
		klog.Warningf("skipping update of status of Ingress rules")
		return
	}

	addrs, err := s.runningAddresses()
	if err != nil {
		klog.ErrorS(err, "error obtaining running IP address")
		return
	}

	if len(addrs) > 1 {
		// leave the job to the next leader
		klog.InfoS("leaving status update for next leader")
		return
	}

	if s.isRunningMultiplePods() {
		klog.V(2).InfoS("skipping Ingress status update (multiple pods running - another one will be elected as master)")
		return
	}

	klog.InfoS("removing value from ingress status", "address", addrs)
	s.updateStatus([]apiv1.LoadBalancerIngress{})
}


func (s *statusSync) sync(key interface{}) error {
	if s.syncQueue.IsShuttingDown() {
		klog.V(2).InfoS("skipping Ingress status update (shutting down in progress)")
		return nil
	}

	addrs, err := s.runningAddresses()
	if err != nil {
		return err
	}
	s.updateStatus(sliceToStatus(addrs))

	return nil
}

func (s statusSync) keyfunc(input interface{}) (interface{}, error) {
	return input, nil
}

// NewStatusSyncer returns a new Syncer instance
func NewStatusSyncer(config Config) Syncer {
	st := statusSync{
		Config: config,
	}
	st.syncQueue = task.NewCustomTaskQueue(st.sync, st.keyfunc)

	return st
}

// runningAddresses returns a list of IP addresses and/or FQDN where the
// ingress controller is currently running
func (s *statusSync) runningAddresses() ([]string, error) {
	if s.PublishStatusAddress != "" {
		return []string{s.PublishStatusAddress}, nil
	}

	if s.PublishService != "" {
		return statusAddressFromService(s.PublishService, s.Client)
	}

	// get information about all the pods running the ingress controller
	pods, err := s.Client.CoreV1().Pods(controller.IngressPodDetails.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(controller.IngressPodDetails.Labels).String(),
	})
	if err != nil {
		return nil, err
	}

	addrs := make([]string, 0)
	for i := range pods.Items {
		pod := pods.Items[i]
		// only Running pods are valid
		if pod.Status.Phase != apiv1.PodRunning {
			continue
		}

		// only Ready pods are valid
		isPodReady := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == apiv1.PodReady && cond.Status == apiv1.ConditionTrue {
				isPodReady = true
				break
			}
		}

		if !isPodReady {
			klog.InfoS("POD is not ready", "pod", klog.KObj(&pod), "node", pod.Spec.NodeName)
			continue
		}

		name := ingress.GetNodeIPOrName(s.Client, pod.Spec.NodeName, s.UseNodeInternalIP)
		if !stringInSlice(name, addrs) {
			addrs = append(addrs, name)
		}
	}

	return addrs, nil
}

func (s *statusSync) isRunningMultiplePods() bool {
	pods, err := s.Client.CoreV1().Pods(controller.IngressPodDetails.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(controller.IngressPodDetails.Labels).String(),
	})
	if err != nil {
		return false
	}

	return len(pods.Items) > 1
}

// sliceToStatus converts a slice of IP and/or hostnames to LoadBalancerIngress
func sliceToStatus(endpoints []string) []apiv1.LoadBalancerIngress {
	lbi := []apiv1.LoadBalancerIngress{}
	for _, ep := range endpoints {
		if net.ParseIP(ep) == nil {
			lbi = append(lbi, apiv1.LoadBalancerIngress{Hostname: ep})
		} else {
			lbi = append(lbi, apiv1.LoadBalancerIngress{IP: ep})
		}
	}

	sort.SliceStable(lbi, func(a, b int) bool {
		return lbi[a].IP < lbi[b].IP
	})

	return lbi
}

// updateStatus changes the status information of Ingress rules
func (s *statusSync) updateStatus(newIngressPoint []apiv1.LoadBalancerIngress) {
	ings := s.IngressLister.ListIngresses()

	p := pool.NewLimited(10)
	defer p.Close()

	batch := p.Batch()
	sort.SliceStable(newIngressPoint, lessLoadBalancerIngress(newIngressPoint))

	for _, ing := range ings {
		curIPs := ing.Status.LoadBalancer.Ingress
		sort.SliceStable(curIPs, lessLoadBalancerIngress(curIPs))
		if ingressSliceEqual(curIPs, newIngressPoint) {
			klog.V(3).InfoS("skipping update of Ingress (no change)", "namespace", ing.Namespace, "ingress", ing.Name)
			continue
		}

		batch.Queue(runUpdate(ing, newIngressPoint, s.Client))
	}

	batch.QueueComplete()
	batch.WaitAll()
}

func runUpdate(ing *ingress.Ingress, status []apiv1.LoadBalancerIngress,
	client kubernetes.Interface) pool.WorkFunc {
	return func(wu pool.WorkUnit) (interface{}, error) {
		if wu.IsCancelled() {
			return nil, nil
		}

		ingClient := client.NetworkingV1beta1().Ingresses(ing.Namespace)
		currIng, err := ingClient.Get(context.TODO(), ing.Name, metav1.GetOptions{})
		if err != nil {
			return nil, errors.Wrap(err, fmt.Sprintf("unexpected error searching Ingress %v/%v", ing.Namespace, ing.Name))
		}

		klog.InfoS("updating Ingress status", "namespace", currIng.Namespace, "ingress", currIng.Name, "currentValue", currIng.Status.LoadBalancer.Ingress, "newValue", status)
		currIng.Status.LoadBalancer.Ingress = status
		_, err = ingClient.UpdateStatus(context.TODO(), currIng, metav1.UpdateOptions{})
		if err != nil {
			klog.Warningf("error updating ingress rule: %v", err)
		}

		return true, nil
	}
}

func lessLoadBalancerIngress(addrs []apiv1.LoadBalancerIngress) func(int, int) bool {
	return func(a, b int) bool {
		switch strings.Compare(addrs[a].Hostname, addrs[b].Hostname) {
		case -1:
			return true
		case 1:
			return false
		}
		return addrs[a].IP < addrs[b].IP
	}
}

func ingressSliceEqual(lhs, rhs []apiv1.LoadBalancerIngress) bool {
	if len(lhs) != len(rhs) {
		return false
	}

	for i := range lhs {
		if lhs[i].IP != rhs[i].IP {
			return false
		}
		if lhs[i].Hostname != rhs[i].Hostname {
			return false
		}
	}

	return true
}

func statusAddressFromService(service string, kubeClient kubernetes.Interface) ([]string, error) {
	ns, name, _ := controller.ParseNameNS(service)
	svc, err := kubeClient.CoreV1().Services(ns).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	switch svc.Spec.Type {
	case apiv1.ServiceTypeExternalName:
		return []string{svc.Spec.ExternalName}, nil
	case apiv1.ServiceTypeClusterIP:
		return []string{svc.Spec.ClusterIP}, nil
	case apiv1.ServiceTypeNodePort:
		addresses := sets.NewString()
		if svc.Spec.ExternalIPs != nil {
			addresses.Insert(svc.Spec.ExternalIPs...)
		} else {
			addresses.Insert(svc.Spec.ClusterIP)
		}
		return addresses.List(), nil
	case apiv1.ServiceTypeLoadBalancer:
		addresses := sets.NewString()
		for _, ip := range svc.Status.LoadBalancer.Ingress {
			if ip.IP == "" {
				addresses.Insert(ip.Hostname)
			} else {
				addresses.Insert(ip.IP)
			}
		}

		addresses.Insert(svc.Spec.ExternalIPs...)

		return addresses.List(), nil
	}

	return nil, fmt.Errorf("unable to extract IP address/es from service %v", service)
}

// stringInSlice returns true if s is in list
func stringInSlice(s string, list []string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}

// NewGetStatusRequest creates a new GET request to the internal NGINX status server
func NewGetStatusRequest(path string) (int, []byte, error) {
	url := fmt.Sprintf("http://127.0.0.1:%v%v", StatusPort, path)

	client := http.Client{}
	res, err := client.Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return 0, nil, err
	}

	return res.StatusCode, data, nil
}
