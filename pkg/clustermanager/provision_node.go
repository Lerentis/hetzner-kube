package clustermanager

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const maxErrors = 3

// NodeProvisioner provisions all basic packages to install docker or crio, kubernetes and wireguard
type NodeProvisioner struct {
	clusterName       string
	node              Node
	communicator      NodeCommunicator
	eventService      EventService
	kubernetesVersion string
	crioEnabeld       bool
}

// NewNodeProvisioner creates a NodeProvisioner instance
func NewNodeProvisioner(node Node, manager *Manager) *NodeProvisioner {
	return &NodeProvisioner{
		clusterName:       manager.clusterName,
		node:              node,
		communicator:      manager.nodeCommunicator,
		eventService:      manager.eventService,
		kubernetesVersion: manager.Cluster().KubernetesVersion,
		crioEnabeld:       manager.crioEnabeld,
	}
}

// Provision performs all steps to provision a node
func (provisioner *NodeProvisioner) Provision(node Node, communicator NodeCommunicator, eventService EventService) error {
	var err error
	errorCount := 0

	for !provisioner.packagesAreInstalled(node, communicator) {

		for err := provisioner.prepareAndInstall(); err != nil; {
			errorCount++

			if errorCount > maxErrors {
				return err
			}
		}

	}

	if err != nil {
		return err
	}

	eventService.AddEvent(node.Name, "packages installed")

	return provisioner.disableSwap()
}

func (provisioner *NodeProvisioner) packagesAreInstalled(node Node, communicator NodeCommunicator) bool {
	out, err := communicator.RunCmd(node, "type -p kubeadm > /dev/null &> /dev/null; echo $?")
	if err != nil {
		return false
	}

	if strings.TrimSpace(out) == "0" {
		return true
	}
	return false
}

func (provisioner *NodeProvisioner) prepareAndInstall() error {

	err := provisioner.waitForCloudInitCompletion()
	if err != nil {
		return err
	}
	err = provisioner.installTransportTools()
	if err != nil {
		return err
	}
	err = provisioner.preparePackages()
	if err != nil {
		return err
	}
	err = provisioner.updateAndInstall()
	if err != nil {
		return err
	}
	err = provisioner.setSystemWideEnvironment()
	if err != nil {
		return err
	}

	return nil
}

func (provisioner *NodeProvisioner) disableSwap() error {
	provisioner.eventService.AddEvent(provisioner.node.Name, "disabling swap")

	_, err := provisioner.communicator.RunCmd(provisioner.node, "swapoff -a")
	if err != nil {
		return err
	}

	_, err = provisioner.communicator.RunCmd(provisioner.node, "sed -i '/ swap / s/^/#/' /etc/fstab")
	return err
}

func (provisioner *NodeProvisioner) waitForCloudInitCompletion() error {

	provisioner.eventService.AddEvent(provisioner.node.Name, "waiting for cloud-init completion")
	var err error

	// define smal bash script to check if /var/lib/cloud/instance/boot-finished exist
	// this file created only when cloud-init finished its tasks
	cloudInitScript := `
#!/bin/bash

# timout is 10 min, return true immediately if ok, otherwise wait timout
# if cloud-init not very complex usually takes 2-3 min to completion
for i in {1..200}
do
  if [ -f /var/lib/cloud/instance/boot-finished ]; then
    exit 0
  fi
  sleep 3
done
exit 127
    `

	err = provisioner.communicator.WriteFile(provisioner.node, "/root/cloud-init-status-check.sh", cloudInitScript, AllExecute)
	if err != nil {
		return err
	}

	for i := 0; i < 10; i++ {
		time.Sleep(3 * time.Second)
		_, err = provisioner.communicator.RunCmd(provisioner.node, "/root/cloud-init-status-check.sh")
	}
	if err != nil {
		return err
	}

	// remove script when done
	_, err = provisioner.communicator.RunCmd(provisioner.node, "rm -f /root/cloud-init-status-check.sh")
	if err != nil {
		return err
	}

	return nil
}

func (provisioner *NodeProvisioner) installTransportTools() error {

	provisioner.eventService.AddEvent(provisioner.node.Name, "installing transport tools")
	var err error
	for i := 0; i < 10; i++ {
		time.Sleep(3 * time.Second)
		_, err = provisioner.communicator.RunCmd(provisioner.node, "apt-get update && apt-get install -y apt-transport-https ca-certificates curl software-properties-common")
	}
	if err != nil {
		return err
	}

	return nil
}

func (provisioner *NodeProvisioner) preparePackages() error {
	provisioner.eventService.AddEvent(provisioner.node.Name, "prepare packages")

	if provisioner.crioEnabeld {
		error := provisioner.prepareCrio()
		if error != nil {
			return error
		}
	} else {
		error := provisioner.prepareDocker()
		if error != nil {
			return error
		}
	}

	err := provisioner.prepareKubernetes()
	if err != nil {
		return err
	}

	// Wireguard (built into Ubuntu 20.04 kernel already, tools are optional)
	_, err = provisioner.communicator.RunCmd(provisioner.node, "apt install -y wireguard-tools")
	if err != nil {
		return err
	}

	return nil
}

func (provisioner *NodeProvisioner) prepareKubernetes() error {
	// kubernetes
	_, err := provisioner.communicator.RunCmd(provisioner.node, "curl -s https://packages.cloud.google.com/apt/doc/apt-key.gpg | apt-key add -")
	if err != nil {
		return err
	}

	// Repository doesn't have Ubuntu 20.04 (focal), but `kubernetes-xenial` works
	err = provisioner.communicator.WriteFile(provisioner.node, "/etc/apt/sources.list.d/kubernetes.list", `deb http://apt.kubernetes.io/ kubernetes-xenial main`, AllRead)
	if err != nil {
		return err
	}

	return nil
}

func (provisioner *NodeProvisioner) prepareDocker() error {
	// docker-ce
	aptPreferencesDocker := `
Package: docker-ce
Pin: version 19.03.13~3-0~ubuntu-focal
Pin-Priority: 1000
	`
	err := provisioner.communicator.WriteFile(provisioner.node, "/etc/apt/preferences.d/docker-ce", aptPreferencesDocker, AllRead)
	if err != nil {
		return err
	}

	_, err = provisioner.communicator.RunCmd(provisioner.node, `curl -fsSL https://download.docker.com/linux/$(. /etc/os-release; echo "$ID")/gpg | apt-key add -`)
	if err != nil {
		return err
	}

	_, err = provisioner.communicator.RunCmd(provisioner.node, `add-apt-repository "deb https://download.docker.com/linux/$(. /etc/os-release; echo "$ID") $(lsb_release -cs) stable"`)
	if err != nil {
		return err
	}

	return nil
}

func (provisioner *NodeProvisioner) prepareCrio() error {

	version, err := provisioner.communicator.RunCmd(provisioner.node, "lsb_release -rs")
	version = strings.TrimSuffix(version, "\n")
	if err != nil {
		return err
	}

	k8sVersionTokens := strings.Split(provisioner.kubernetesVersion, ".")
	k8sVersion := k8sVersionTokens[0] + "." + k8sVersionTokens[1]

	command := fmt.Sprintf("curl -fsSL https://download.opensuse.org/repositories/devel:kubic:libcontainers:stable:cri-o:%s/%s/Release.key | apt-key add -", k8sVersion, "xUbuntu_"+version)
	_, err = provisioner.communicator.RunCmd(provisioner.node, command)
	if err != nil {
		return err
	}

	command = fmt.Sprintf("curl -fsSL https://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/stable/%s/Release.key | apt-key add -", "xUbuntu_"+version)
	_, err = provisioner.communicator.RunCmd(provisioner.node, command)
	if err != nil {
		return err
	}

	err = provisioner.communicator.WriteFile(provisioner.node, "/etc/apt/sources.list.d/devel:kubic:libcontainers:stable.list", "deb https://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/stable/xUbuntu_"+version+"/ /", AllRead)
	if err != nil {
		return err
	}
	err = provisioner.communicator.WriteFile(provisioner.node, "/etc/apt/sources.list.d/devel:kubic:libcontainers:stable:cri-o"+k8sVersion+".list", "deb http://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/stable:/cri-o:/"+k8sVersion+"/xUbuntu_"+version+"/ /", AllRead)
	if err != nil {
		return err
	}
	err = provisioner.communicator.WriteFile(provisioner.node, "/etc/sysctl.d/99-kubernetes-cri.conf", "net.bridge.bridge-nf-call-iptables  = 1 \n net.ipv4.ip_forward                 = 1 \n net.bridge.bridge-nf-call-ip6tables = 1 \n", AllRead)
	if err != nil {
		return err
	}
	_, err = provisioner.communicator.RunCmd(provisioner.node, "modprobe overlay && modprobe br_netfilter && sysctl --system")
	if err != nil {
		return err
	}
	err = provisioner.communicator.WriteFile(provisioner.node, "/var/lib/kubelet/kubeadm-flags.env", "KUBELET_KUBEADM_ARGS=\"--cgroup-driver=systemd --container-runtime=remote --container-runtime-endpoint=unix:///var/run/crio/crio.sock\"", AllRead)
	if err != nil {
		return err
	}

	return nil
}

func (provisioner *NodeProvisioner) updateAndInstall() error {
	provisioner.eventService.AddEvent(provisioner.node.Name, "updating packages")
	_, err := provisioner.communicator.RunCmd(provisioner.node, "apt-get update")
	if err != nil {
		return err
	}

	provisioner.eventService.AddEvent(provisioner.node.Name, "installing packages")
	command := ""
	if provisioner.crioEnabeld {
		command = fmt.Sprintf("apt-get install -y cri-o cri-o-runc kubelet=%s-00 kubeadm=%s-00 kubectl=%s-00 kubernetes-cni=0.8.7-00 wireguard linux-headers-generic linux-headers-virtual && systemctl enable crio.service && systemctl start crio.service",
			provisioner.kubernetesVersion, provisioner.kubernetesVersion, provisioner.kubernetesVersion)
	} else {
		command = fmt.Sprintf("apt-get install -y docker-ce kubelet=%s-00 kubeadm=%s-00 kubectl=%s-00 kubernetes-cni=0.8.7-00 wireguard linux-headers-generic linux-headers-virtual",
			provisioner.kubernetesVersion, provisioner.kubernetesVersion, provisioner.kubernetesVersion)
	}

	_, err = provisioner.communicator.RunCmd(provisioner.node, command)
	if err != nil {
		return err
	}

	return nil
}

// Last step because otherwise we need create script to check if variables already set and replaces them
// As soon as it is last step we are ok to set them in basic way
func (provisioner *NodeProvisioner) setSystemWideEnvironment() error {
	provisioner.eventService.AddEvent(provisioner.node.Name, "set environment variables")
	var err error

	// set HETZNER_KUBE_MASTER
	_, err = provisioner.communicator.RunCmd(provisioner.node, fmt.Sprintf("echo \"HETZNER_KUBE_MASTER=%s\" >> /etc/environment", strconv.FormatBool(provisioner.node.IsMaster)))
	if err != nil {
		return err
	}

	// set HETZNER_KUBE_CLUSTER
	_, err = provisioner.communicator.RunCmd(provisioner.node, fmt.Sprintf("echo \"HETZNER_KUBE_CLUSTER=%s\" >> /etc/environment", provisioner.clusterName))
	if err != nil {
		return err
	}

	return nil
}
