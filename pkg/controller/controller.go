package controller

import (
	"os"
	"sync"
	"time"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
)

type Controller struct {
	client         kubernetes.Interface
	nodePVMap      map[string][]*v1.PersistentVolume
	nodePVLock     *sync.Mutex
	nodeController cache.Controller
	podController  cache.Controller
}

func NewNodeFencingController(client kubernetes.Interface) *Controller {
	c := &Controller{
		client:     client,
		nodePVMap:  make(map[string][]*v1.PersistentVolume),
		nodePVLock: &sync.Mutex{},
	}

	nodeListWatcher := cache.NewListWatchFromClient(
		client.Core().RESTClient(),
		"nodes",
		v1.NamespaceAll,
		fields.Everything())

	_, nodeController := cache.NewInformer(
		nodeListWatcher,
		&v1.Node{},
		time.Minute*60,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onNodeAdd,
			UpdateFunc: c.onNodeUpdate,
			DeleteFunc: c.onNodeDelete,
		},
	)

	c.nodeController = nodeController

	podListWatcher := cache.NewListWatchFromClient(
		client.Core().RESTClient(),
		"pods",
		v1.NamespaceAll,
		fields.Everything())

	_, podController := cache.NewInformer(
		podListWatcher,
		&v1.Pod{},
		time.Minute*60,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPodAdd,
			UpdateFunc: c.onPodUpdate,
			DeleteFunc: c.onPodDelete,
		},
	)

	c.podController = podController

	return c
}

func (c *Controller) Run(ctx <-chan struct{}) {
	glog.Infof("pod controller starting")
	go c.podController.Run(ctx)
	wait.Poll(time.Second, 5*time.Minute, func() (bool, error) {
		return c.podController.HasSynced(), nil
	})
	if !c.podController.HasSynced() {
		glog.Errorf("pod informer controller initial sync timeout")
		os.Exit(1)
	}

	glog.Infof("node controller starting")
	go c.nodeController.Run(ctx)
	glog.Infof("Waiting for informer initial sync")
	wait.Poll(time.Second, 5*time.Minute, func() (bool, error) {
		return c.nodeController.HasSynced(), nil
	})
	if !c.nodeController.HasSynced() {
		glog.Errorf("node informer controller initial sync timeout")
		os.Exit(1)
	}
}

func (c *Controller) onNodeAdd(obj interface{}) {
	node := obj.(*v1.Node)
	nodeName := node.Name
	glog.Infof("add node: %s", nodeName)
	status := node.Status
	conditions := status.Conditions

	for _, condition := range conditions {
		if v1.NodeReady == condition.Type && v1.ConditionUnknown == condition.Status {
			glog.Warningf("node %s Ready status is unknown", nodeName)
			if len(c.nodePVMap[nodeName]) > 0 {
				glog.Warningf("PVs on node %s:", nodeName)
				for _, pv := range c.nodePVMap[nodeName] {
					glog.Warningf("\t%v:", pv.Name)
				}
			}
		}
	}

}

func (c *Controller) onNodeUpdate(oldObj, newObj interface{}) {
	oldNode := oldObj.(*v1.Node)
	newNode := newObj.(*v1.Node)
	nodeName := oldNode.Name
	glog.V(4).Infof("update node: %s/%s", oldNode.Name, newNode.Name)
	newStatus := newNode.Status
	newConditions := newStatus.Conditions
	oldStatus := oldNode.Status
	oldConditions := oldStatus.Conditions

	for _, newCondition := range newConditions {
		found := false
		for _, oldCondition := range oldConditions {
			if newCondition.LastTransitionTime == oldCondition.LastTransitionTime {
				found = true
				break
			}
		}
		if !found {
			if v1.NodeReady == newCondition.Type && v1.ConditionUnknown == newCondition.Status {
				glog.Warningf("node %s Ready status is unknown", nodeName)
				if len(c.nodePVMap[nodeName]) > 0 {
					glog.Warningf("PVs on node %s:", nodeName)
					for _, pv := range c.nodePVMap[nodeName] {
						glog.Warningf("\t%v:", pv.Name)
					}
				}
			}
		}
	}

}

func (c *Controller) onNodeDelete(obj interface{}) {
	node := obj.(*v1.Node)
	glog.Infof("delete node: %s", node.Name)
}

func (c *Controller) onPodAdd(obj interface{}) {
	pod := obj.(*v1.Pod)
	glog.V(4).Infof("add pod: %s", pod.Name)
	c.updateNodePV(pod, true)
}

func (c *Controller) onPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)
	glog.V(4).Infof("update pod: %s/%s", oldPod.Name, newPod.Name)
	//FIXME: should evaluate PV status between old and new pod before update node pv map
	c.updateNodePV(newPod, true)
}

func (c *Controller) onPodDelete(obj interface{}) {
	pod := obj.(*v1.Pod)
	glog.V(4).Infof("delete pod: %s", pod.Name)
	c.updateNodePV(pod, false)
}

func (c *Controller) updateNodePV(pod *v1.Pod, toAdd bool) {
	node := pod.Spec.NodeName
	if len(node) == 0 {
		return
	}
	podPrinted := false
	for _, vol := range pod.Spec.Volumes {
		if vol.VolumeSource.PersistentVolumeClaim != nil {
			if !podPrinted {
				glog.V(4).Infof("Pod: %s/%s", pod.Namespace, pod.Name)
				glog.V(4).Infof("\tnode: %s", node)
				podPrinted = true
			}
			pvcName := vol.VolumeSource.PersistentVolumeClaim.ClaimName
			glog.V(4).Infof("\tpvc: %v", pvcName)
			pvc, err := c.client.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(pvcName, metav1.GetOptions{})
			if err == nil {
				pvName := pvc.Spec.VolumeName
				if len(pvName) != 0 {
					glog.V(4).Infof("\tpv: %v", pvName)
					pv, err := c.client.CoreV1().PersistentVolumes().Get(pvName, metav1.GetOptions{})
					if err == nil {
						c.updateNodePVMap(node, pv, toAdd)
					}
				}
			}
		}
	}
}

func (c *Controller) updateNodePVMap(node string, pv *v1.PersistentVolume, toAdd bool) {
	c.nodePVLock.Lock()
	defer c.nodePVLock.Unlock()
	for i, p := range c.nodePVMap[node] {
		if p != nil && pv != nil && p.Name == pv.Name {
			if toAdd {
				// already in the map, not to add
				return
			}
			c.nodePVMap[node][i] = c.nodePVMap[node][len(c.nodePVMap[node])-1]
			c.nodePVMap[node] = c.nodePVMap[node][:len(c.nodePVMap[node])-1]
			glog.V(6).Infof("node %s pv map: %v", node, c.nodePVMap[node])
			return
		}
	}
	c.nodePVMap[node] = append(c.nodePVMap[node], pv)
	glog.V(6).Infof("node %s pv map: %v", node, c.nodePVMap[node])
}