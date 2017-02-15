package cluster

import (
	"errors"
	"github.com/absolute8511/ZanRedisDB/common"
	"sync/atomic"
	"time"
)

// some API for outside
func (self *PDCoordinator) GetAllPDNodes() ([]NodeInfo, error) {
	return self.register.GetAllPDNodes()
}

func (self *PDCoordinator) GetPDLeader() NodeInfo {
	return self.leaderNode
}

func (self *PDCoordinator) SetClusterUpgradeState(upgrading bool) error {
	if self.leaderNode.GetID() != self.myNode.GetID() {
		coordLog.Infof("not leader while delete namespace")
		return ErrNotLeader
	}

	if upgrading {
		if !atomic.CompareAndSwapInt32(&self.isUpgrading, 0, 1) {
			coordLog.Infof("the cluster state is already upgrading")
			return nil
		}
		coordLog.Infof("the cluster state has been changed to upgrading")
	} else {
		if !atomic.CompareAndSwapInt32(&self.isUpgrading, 1, 0) {
			return nil
		}
		coordLog.Infof("the cluster state has been changed to normal")
		self.triggerCheckNamespaces("", 0, time.Second)
	}
	return nil
}

func (self *PDCoordinator) MarkNodeAsRemoving(nid string) error {
	if self.leaderNode.GetID() != self.myNode.GetID() {
		coordLog.Infof("not leader while delete namespace")
		return ErrNotLeader
	}

	coordLog.Infof("try mark node %v as removed", nid)
	self.nodesMutex.Lock()
	newRemovingNodes := make(map[string]string)
	if _, ok := self.removingNodes[nid]; ok {
		coordLog.Infof("already mark as removing")
	} else {
		newRemovingNodes[nid] = "marked"
		for id, removeState := range self.removingNodes {
			newRemovingNodes[id] = removeState
		}
		self.removingNodes = newRemovingNodes
	}
	self.nodesMutex.Unlock()
	return nil
}

func (self *PDCoordinator) DeleteNamespace(ns string, partition string) error {
	if self.leaderNode.GetID() != self.myNode.GetID() {
		coordLog.Infof("not leader while delete namespace")
		return ErrNotLeader
	}

	begin := time.Now()
	for !atomic.CompareAndSwapInt32(&self.doChecking, 0, 1) {
		coordLog.Infof("delete %v waiting check namespace finish", ns)
		time.Sleep(time.Millisecond * 200)
		if time.Since(begin) > time.Second*5 {
			return ErrClusterUnstable
		}
	}
	defer atomic.StoreInt32(&self.doChecking, 0)
	coordLog.Infof("delete namespace: %v, with partition: %v", ns, partition)
	if ok, err := self.register.IsExistNamespace(ns); !ok {
		coordLog.Infof("no namespace : %v", err)
		return ErrKeyNotFound
	}

	if partition == "**" {
		// delete all
		meta, err := self.register.GetNamespaceMetaInfo(ns)
		if err != nil {
			coordLog.Infof("failed to get meta for namespace: %v", err)
		}
		for pid := 0; pid < meta.PartitionNum; pid++ {
			err := self.deleteNamespacePartition(ns, pid)
			if err != nil {
				coordLog.Infof("failed to delete namespace partition %v for namespace: %v, err:%v", pid, ns, err)
			}
		}
		err = self.register.DeleteWholeNamespace(ns)
		if err != nil {
			coordLog.Infof("failed to delete whole namespace: %v : %v", ns, err)
		}
	}
	return nil
}

func (self *PDCoordinator) deleteNamespacePartition(namespace string, pid int) error {
	commonErr := self.register.DeleteNamespacePart(namespace, pid)
	if commonErr != nil {
		coordLog.Infof("failed to delete the namespace info : %v", commonErr)
		return commonErr
	}
	return nil
}

func (self *PDCoordinator) ChangeNamespaceMetaParam(namespace string, newReplicator int) error {
	if self.leaderNode.GetID() != self.myNode.GetID() {
		coordLog.Infof("not leader while create namespace")
		return ErrNotLeader
	}

	if !common.IsValidNamespaceName(namespace) {
		return errors.New("invalid namespace name")
	}

	if newReplicator > 5 {
		return errors.New("max replicator allowed exceed")
	}

	var meta NamespaceMetaInfo
	if ok, _ := self.register.IsExistNamespace(namespace); !ok {
		coordLog.Infof("namespace not exist %v :%v", namespace)
		return ErrNamespaceNotCreated.ToErrorType()
	} else {
		oldMeta, err := self.register.GetNamespaceMetaInfo(namespace)
		if err != nil {
			coordLog.Infof("get namespace key %v failed :%v", namespace, err)
			return err
		}
		currentNodes := self.getCurrentNodes()
		meta = oldMeta
		if newReplicator > 0 {
			meta.Replica = newReplicator
		}
		err = self.updateNamespaceMeta(currentNodes, namespace, &meta)
		if err != nil {
			return err
		}
		self.triggerCheckNamespaces("", 0, 0)
	}
	return nil
}

func (self *PDCoordinator) updateNamespaceMeta(currentNodes map[string]NodeInfo, namespace string, meta *NamespaceMetaInfo) error {
	coordLog.Infof("update namespace: %v, with meta: %v", namespace, meta)

	if len(currentNodes) < meta.Replica {
		coordLog.Infof("nodes %v is less than replica  %v", len(currentNodes), meta)
		return ErrNodeUnavailable.ToErrorType()
	}
	return self.register.UpdateNamespaceMetaInfo(namespace, meta, meta.MetaEpoch)
}

func (self *PDCoordinator) CreateNamespace(namespace string, meta NamespaceMetaInfo) error {
	if self.leaderNode.GetID() != self.myNode.GetID() {
		coordLog.Infof("not leader while create namespace")
		return ErrNotLeader
	}

	if !common.IsValidNamespaceName(namespace) {
		return errors.New("invalid namespace name")
	}

	if meta.PartitionNum >= common.MAX_PARTITION_NUM {
		return errors.New("max partition allowed exceed")
	}

	currentNodes := self.getCurrentNodes()
	if len(currentNodes) < meta.Replica {
		coordLog.Infof("nodes %v is less than replica %v", len(currentNodes), meta)
		return ErrNodeUnavailable.ToErrorType()
	}
	if ok, _ := self.register.IsExistNamespace(namespace); !ok {
		meta.MagicCode = time.Now().UnixNano()
		err := self.register.CreateNamespace(namespace, &meta)
		if err != nil {
			coordLog.Infof("create namespace key %v failed :%v", namespace, err)
			return err
		}
	} else {
		coordLog.Warningf("namespace already exist :%v ", namespace)
		return ErrAlreadyExist
	}
	coordLog.Infof("create namespace: %v, with meta: %v", namespace, meta)

	return self.checkAndUpdateNamespacePartitions(currentNodes, namespace, meta)
}

func (self *PDCoordinator) checkAndUpdateNamespacePartitions(currentNodes map[string]NodeInfo,
	namespace string, meta NamespaceMetaInfo) error {
	existPart := make(map[int]*PartitionMetaInfo)
	for i := 0; i < meta.PartitionNum; i++ {
		err := self.register.CreateNamespacePartition(namespace, i)
		if err != nil {
			coordLog.Warningf("failed to create namespace %v-%v: %v", namespace, i, err)
			// handle already exist
			t, err := self.register.GetNamespacePartInfo(namespace, i)
			if err != nil {
				coordLog.Warningf("exist namespace partition failed to get info: %v", err)
				if err != ErrKeyNotFound {
					return err
				}
			} else {
				coordLog.Infof("create namespace partition already exist %v-%v", namespace, i)
				existPart[i] = t
			}
		}
	}
	isrList, err := self.dpm.allocNamespaceRaftNodes(namespace, currentNodes, meta.Replica, meta.PartitionNum, existPart)
	if err != nil {
		coordLog.Infof("failed to alloc nodes for namespace: %v", err)
		return err.ToErrorType()
	}
	if len(isrList) != meta.PartitionNum {
		return ErrNodeUnavailable.ToErrorType()
	}
	//meta.MaxRaftID += meta.PartitionNum*meta.Replica
	for i := 0; i < meta.PartitionNum; i++ {
		if _, ok := existPart[i]; ok {
			continue
		}
		var tmpReplicaInfo PartitionReplicaInfo
		tmpReplicaInfo.RaftNodes = isrList[i]
		// each new raft node should use the new raft id

		commonErr := self.register.UpdateNamespacePartReplicaInfo(namespace, i, &tmpReplicaInfo, tmpReplicaInfo.Epoch)
		if commonErr != nil {
			coordLog.Infof("failed update info for namespace : %v-%v, %v", namespace, i, commonErr)
			continue
		}
		tmpNamespaceInfo := PartitionMetaInfo{}
		tmpNamespaceInfo.Name = namespace
		tmpNamespaceInfo.Partition = i
		tmpNamespaceInfo.NamespaceMetaInfo = meta
		tmpNamespaceInfo.PartitionReplicaInfo = tmpReplicaInfo
	}
	self.triggerCheckNamespaces("", 0, time.Millisecond*500)
	return nil
}
