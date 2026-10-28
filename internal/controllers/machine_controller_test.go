// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"os"
	"path/filepath"
	"time"

	"github.com/digitalocean/go-libvirt"
	"github.com/ironcore-dev/libvirt-provider/api"
	libvirtutils "github.com/ironcore-dev/libvirt-provider/internal/libvirt/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	utilstrings "k8s.io/utils/strings"
	"libvirt.org/go/libvirtxml"
)

var _ = Describe("MachineController", func() {
	Context("Machine Lifecycle", func() {
		var machineID string

		It("should create and reconcile a machine", func(ctx SpecContext) {
			By("creating a machine in the store")
			machine, err := createMachine(api.MachineSpec{
				Power:       api.PowerStatePowerOn,
				Cpu:         4,
				MemoryBytes: 8589934592, // 8GB
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(machine).NotTo(BeNil())
			Expect(machine.ID).NotTo(BeEmpty())

			GinkgoWriter.Printf("Created machine: ID=%s\n", machineID)

			DeferCleanup(cleanupMachine(machine.ID))

			By("ensuring domain and domain XML is created for machine")
			var domain libvirt.Domain
			Eventually(func() error {
				domain, err = libvirtConn.DomainLookupByUUID(libvirtutils.UUIDStringToBytes(machine.ID))
				return err
			}).Should(Succeed())
			domainXMLData, err := libvirtConn.DomainGetXMLDesc(domain, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(domainXMLData).NotTo(BeEmpty())

			By("ensuring domain for machine is in running state")
			Eventually(func(g Gomega) libvirt.DomainState {
				domainState, _, err := libvirtConn.DomainGetState(domain, 0)
				g.Expect(err).NotTo(HaveOccurred())
				return libvirt.DomainState(domainState)
			}).Should(Equal(libvirt.DomainRunning))

			By("ensuring machine is in running state")
			Eventually(func(g Gomega) {
				m, err := getMachine(machine.ID)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(m.Status.State).To(Equal(api.MachineStateRunning))
			}).Should(Succeed())

			By("ensuring the console output is mirrored to a log file")
			domainXML := &libvirtxml.Domain{}
			Expect(domainXML.Unmarshal(domainXMLData)).To(Succeed())
			Expect(domainXML.Devices).NotTo(BeNil())
			Expect(domainXML.Devices.Serials).To(HaveLen(1))
			consoleLog := domainXML.Devices.Serials[0].Log
			Expect(consoleLog).NotTo(BeNil())
			Expect(consoleLog.File).To(Equal(providerHost.MachineConsoleLogFile(machine.ID)))
			Expect(consoleLog.Append).To(Equal("on"))
			Expect(providerHost.MachineLogsDir(machine.ID)).To(BeADirectory())
			Eventually(providerHost.MachineConsoleLogFile(machine.ID)).Should(BeAnExistingFile())
		})

		It("should handle machine with boot image and network interface", func(ctx SpecContext) {
			By("creating a machine with boot image and network interface")
			image := osImage
			machine, err := createMachine(api.MachineSpec{
				Power:       api.PowerStatePowerOn,
				Cpu:         2,
				MemoryBytes: 2147483648, // 2GB
				Volumes: []*api.VolumeSpec{
					{
						Name: "disk-1",
						LocalDisk: &api.LocalDiskSpec{
							Image: &image,
						},
						Device: "oda",
					},
				},
				NetworkInterfaces: []*api.NetworkInterfaceSpec{
					{
						Name: "nic-1",
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(machine).NotTo(BeNil())

			GinkgoWriter.Printf("Created machine with NIC: ID=%s\n", machine.ID)

			By("verifying network interface is configured")
			Eventually(func(g Gomega) {
				m, err := getMachine(machine.ID)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(m.Spec.NetworkInterfaces).To(HaveLen(1))
				g.Expect(m.Spec.NetworkInterfaces[0].Name).To(Equal("nic-1"))
			}).Should(Succeed())

			DeferCleanup(cleanupMachine(machine.ID))

			By("ensuring domain and domain XML is created for machine")
			var domain libvirt.Domain
			Eventually(func() error {
				domain, err = libvirtConn.DomainLookupByUUID(libvirtutils.UUIDStringToBytes(machine.ID))
				return err
			}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
			domainXMLData, err := libvirtConn.DomainGetXMLDesc(domain, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(domainXMLData).NotTo(BeEmpty())

			By("ensuring domain for machine is in running state")
			Eventually(func(g Gomega) libvirt.DomainState {
				domainState, _, err := libvirtConn.DomainGetState(domain, 0)
				g.Expect(err).NotTo(HaveOccurred())
				return libvirt.DomainState(domainState)
			}).Should(Equal(libvirt.DomainRunning))

			By("ensuring machine is in running state and other status fields have been updated")
			Eventually(func(g Gomega) *api.MachineStatus {
				m, err := getMachine(machine.ID)
				g.Expect(err).NotTo(HaveOccurred())
				return &m.Status
			}).Should(SatisfyAll(
				HaveField("ImageRef", BeEmpty()),
				HaveField("VolumeStatus", ContainElement(SatisfyAll(
					HaveField("Name", Equal("disk-1")),
					HaveField("Handle", Equal("libvirt-provider.ironcore.dev/local-disk/disk-1")),
					HaveField("State", Equal(api.VolumeStateAttached)),
				))),
				HaveField("NetworkInterfaceStatus", ContainElement(api.NetworkInterfaceStatus{
					Name:  "nic-1",
					State: api.NetworkInterfaceStateAttached,
				})),
				HaveField("State", Equal(api.MachineStateRunning)),
			))
		})

		It("should handle machine with boot image and size limit set", func(ctx SpecContext) {
			By("creating a machine with boot volume")
			image := osImage

			machine, err := createMachine(api.MachineSpec{
				Power:       api.PowerStatePowerOn,
				Cpu:         4,
				MemoryBytes: 2147483648,
				Volumes: []*api.VolumeSpec{
					{
						Name: "disk-0",
						LocalDisk: &api.LocalDiskSpec{
							Image: &image,
							Size:  10 * 1024 * 1024 * 1024, // 10GB
						},
						Device: "oda",
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(machine).NotTo(BeNil())

			DeferCleanup(cleanupMachine(machine.ID))

			diskPath := filepath.Join(
				providerHost.MachineVolumeDir(
					machine.ID,
					utilstrings.EscapeQualifiedName("libvirt-provider.ironcore.dev/local-disk"),
					"disk-0",
				),
				"disk.raw",
			)
			var stat os.FileInfo
			Eventually(func(g Gomega) {
				stat, err = os.Stat(diskPath)
				g.Expect(err).NotTo(HaveOccurred())
			}).
				WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).
				Should(Succeed())

			Expect(stat.Size()).To(BeNumerically("==", int64(10*1024*1024*1024)),
				"disc size should be as configured")
		})

		It("should update machine power state", func(ctx SpecContext) {
			By("creating a machine in powered on state")
			machine, err := createMachine(api.MachineSpec{
				Power:       api.PowerStatePowerOn,
				Cpu:         2,
				MemoryBytes: 2147483648,
			})
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(cleanupMachine(machine.ID))

			By("ensuring domain and domain XML is created for machine")
			var domain libvirt.Domain
			Eventually(func() error {
				domain, err = libvirtConn.DomainLookupByUUID(libvirtutils.UUIDStringToBytes(machine.ID))
				return err
			}).Should(Succeed())
			domainXMLData, err := libvirtConn.DomainGetXMLDesc(domain, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(domainXMLData).NotTo(BeEmpty())

			By("ensuring domain for machine is in running state")
			Eventually(func(g Gomega) libvirt.DomainState {
				domainState, _, err := libvirtConn.DomainGetState(domain, 0)
				g.Expect(err).NotTo(HaveOccurred())
				return libvirt.DomainState(domainState)
			}).Should(Equal(libvirt.DomainRunning))

			By("ensuring machine is in running state")
			Eventually(func(g Gomega) {
				m, err := getMachine(machine.ID)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(m.Status.State).To(Equal(api.MachineStateRunning))
			}).Should(Succeed())

			By("updating machine to powered off state")
			machine, err = getMachine(machine.ID)
			Expect(err).NotTo(HaveOccurred())
			machine.Spec.Power = api.PowerStatePowerOff
			updatedMachine, err := updateMachine(machine)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedMachine.Spec.Power).To(Equal(api.PowerStatePowerOff))

			By("verifying power state change is persisted")
			Eventually(func(g Gomega) {
				m, err := getMachine(machine.ID)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(m.Spec.Power).To(Equal(api.PowerStatePowerOff))
			}).Should(Succeed())

			By("ensuring the powered-off domain stops")
			domainConfig := &libvirtxml.Domain{}
			Expect(domainConfig.Unmarshal(domainXMLData)).To(Succeed())
			Expect(domainConfig.Type).To(BeElementOf("kvm", "qemu"))
			GinkgoWriter.Printf("POWER_DOMAIN machine_id=%s xml=%s\n", machine.ID, domainXMLData)
			assertPoweredOff := func(g Gomega) {
				currentMachine, err := getMachine(machine.ID)
				g.Expect(err).NotTo(HaveOccurred())
				currentDomain, err := libvirtConn.DomainLookupByUUID(libvirtutils.UUIDStringToBytes(machine.ID))
				if libvirt.IsNotFound(err) {
					return
				}
				g.Expect(err).NotTo(HaveOccurred())
				domainState, _, err := libvirtConn.DomainGetState(currentDomain, 0)
				if libvirt.IsNotFound(err) {
					return
				}
				g.Expect(err).NotTo(HaveOccurred())
				GinkgoWriter.Printf("POWER_SAMPLE at=%s machine_id=%s desired_numeric=%d status=%s domain_state=%d\n", time.Now().UTC().Format(time.RFC3339Nano), machine.ID, currentMachine.Spec.Power, currentMachine.Status.State, domainState)
				g.Expect(currentMachine.Spec.Power).To(Equal(api.PowerStatePowerOff))
				g.Expect(libvirt.DomainState(domainState)).To(Equal(libvirt.DomainShutoff))
			}
			Eventually(assertPoweredOff).WithPolling(time.Second).Should(Succeed())

			By("ensuring execution stays off across a controller resync")
			Consistently(assertPoweredOff).WithTimeout(35 * time.Second).WithPolling(time.Second).Should(Succeed())

			By("ensuring machine status no longer reports running")
			Eventually(func(g Gomega) {
				currentMachine, err := getMachine(machine.ID)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(currentMachine.Spec.Power).To(Equal(api.PowerStatePowerOff))
				g.Expect(currentMachine.Status.State).NotTo(Equal(api.MachineStateRunning))
			}).Should(Succeed())

			By("powering the same machine on again")
			machine, err = getMachine(machine.ID)
			Expect(err).NotTo(HaveOccurred())
			machine.Spec.Power = api.PowerStatePowerOn
			_, err = updateMachine(machine)
			Expect(err).NotTo(HaveOccurred())

			By("ensuring native execution and machine status return to running")
			Eventually(func(g Gomega) {
				currentDomain, err := libvirtConn.DomainLookupByUUID(libvirtutils.UUIDStringToBytes(machine.ID))
				g.Expect(err).NotTo(HaveOccurred())
				domainState, _, err := libvirtConn.DomainGetState(currentDomain, 0)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(libvirt.DomainState(domainState)).To(Equal(libvirt.DomainRunning))
				currentMachine, err := getMachine(machine.ID)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(currentMachine.Spec.Power).To(Equal(api.PowerStatePowerOn))
				g.Expect(currentMachine.Status.State).To(Equal(api.MachineStateRunning))
			}).Should(Succeed())
		})
	})
})
