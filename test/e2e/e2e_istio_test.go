/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/istio-ecosystem/fortsa/test/utils"
)

const (
	istioClusterNameDefault = "fortsa-test-e2e"
	istioOldVersionDefault  = "1.28.4"
	istioNewVersionDefault  = "1.29.0"
	restartWaitTimeout      = 5 * time.Minute
	restartPollInterval     = 2 * time.Second
	fortsaNamespace         = "fortsa-system"
	restartedAtAnnotation   = "fortsa\\.scaffidi\\.net/restartedAt"
)

func istioClusterName(suffix string) string {
	if v := os.Getenv("KIND_CLUSTER"); v != "" {
		return v + "-" + suffix
	}
	return istioClusterNameDefault + "-" + suffix
}

func istioOldVersion() string {
	if v := os.Getenv("ISTIO_OLD_VERSION"); v != "" {
		return v
	}
	return istioOldVersionDefault
}

func istioNewVersion() string {
	if v := os.Getenv("ISTIO_NEW_VERSION"); v != "" {
		return v
	}
	return istioNewVersionDefault
}

func skipIfIstioToolsMissing() {
	for _, cmd := range []string{"kind", "kubectl"} {
		if _, err := exec.LookPath(cmd); err != nil {
			Skip(fmt.Sprintf("%s not found, skipping Istio e2e tests", cmd))
		}
	}
	containerTool := os.Getenv("CONTAINER_TOOL")
	if containerTool == "" {
		containerTool = "docker"
	}
	if _, err := exec.LookPath(containerTool); err != nil {
		Skip(fmt.Sprintf("CONTAINER_TOOL (%s) not found, skipping Istio e2e tests", containerTool))
	}
}

func projectRoot() string {
	dir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())
	return dir
}

func helloWorldManifestPath() string {
	return filepath.Join(projectRoot(), "test", "e2e-scripts", "hello-world.yaml")
}

func configDefaultPath() string {
	return filepath.Join(projectRoot(), "config", "default")
}

func waitForFortsaRestart(namespace, deployment, initialPod string) string {
	var newPod string
	verifyRestart := func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "deployment", deployment,
			"-o", "jsonpath={.spec.template.metadata.annotations."+restartedAtAnnotation+"}",
			"-n", namespace)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).NotTo(BeEmpty(), "restartedAt annotation not yet set")

		cmd = exec.Command("kubectl", "get", "pods", "-l", "app=helloworld",
			"-o", "jsonpath={.items[0].metadata.name}",
			"-n", namespace)
		output, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).NotTo(BeEmpty())
		g.Expect(output).NotTo(Equal(initialPod), "pod has not been replaced yet")
		newPod = output
	}
	Eventually(verifyRestart).WithTimeout(restartWaitTimeout).WithPolling(restartPollInterval).Should(Succeed())
	return newPod
}

func getProxyImage(namespace, podName string) string {
	cmd := exec.Command("kubectl", "get", "pod", podName,
		"-o", `jsonpath={.spec.containers[?(@.name=="istio-proxy")].image}`,
		"-n", namespace)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	return output
}

func setupIstioCluster(clusterName string) (tmpDir string) {
	By("deleting existing kind cluster")
	_ = utils.KindDeleteCluster(clusterName)

	By("creating kind cluster")
	Expect(utils.KindCreateCluster(clusterName, 2*time.Minute)).To(Succeed())

	By("loading fortsa image into kind cluster")
	Expect(utils.LoadImageToKindCluster(projectImage, clusterName)).To(Succeed())

	var err error
	tmpDir, err = os.MkdirTemp("", "fortsa-istio-e2e-")
	Expect(err).NotTo(HaveOccurred())
	return tmpDir
}

func deployFortsa() {
	By("deploying fortsa")
	cmd := exec.Command("kubectl", "apply", "-k", configDefaultPath())
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("waiting for fortsa controller to be ready")
	cmd = exec.Command("kubectl", "rollout", "status", "deployment/fortsa-controller-manager",
		"-n", fortsaNamespace, "--timeout=120s")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	time.Sleep(10 * time.Second)
}

func deployHelloWorldAndWaitForSidecar(namespace, nsLabel string) {
	By("creating namespace and labeling for sidecar injection")
	cmd := exec.Command("kubectl", "create", "namespace", namespace)
	_, _ = utils.Run(cmd)

	cmd = exec.Command("kubectl", "label", "namespace", namespace, nsLabel, "--overwrite")
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("deploying hello-world app")
	cmd = exec.Command("kubectl", "apply", "-f", helloWorldManifestPath(), "-n", namespace)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("waiting for hello-world deployment and sidecar injection")
	cmd = exec.Command("kubectl", "rollout", "status", "deployment/helloworld", "-n", namespace, "--timeout=120s")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("dumping pods across all namespaces")
	cmd = exec.Command("kubectl", "get", "pods", "--all-namespaces")
	output, err := utils.Run(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl get pods failed: %v\n", err)
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "Pods:\n%s\n", output)
	}

	verifySidecar := func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods", "-n", namespace, "-l", "app=helloworld",
			"-o", "jsonpath={.items[0].spec.containers[*].name}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(ContainSubstring("istio-proxy"))
	}
	Eventually(verifySidecar).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
}

func getHelloWorldPodName(namespace string) string {
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace, "-l", "app=helloworld",
		"-o", "jsonpath={.items[0].metadata.name}")
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	return output
}

var _ = Describe("Istio in-place upgrade", Label("Istio"), Ordered, func() {
	var (
		clusterName string
		tmpDir      string
		istioctlNew string
		initialPod  string
		testName    = "in-place-upgrade"
	)

	BeforeAll(func() {
		skipIfIstioToolsMissing()
		clusterName = istioClusterName(testName)
		tmpDir = setupIstioCluster(clusterName)

		By("downloading Istio versions")
		istioDirOld, err := utils.DownloadIstio(istioOldVersion(), tmpDir)
		Expect(err).NotTo(HaveOccurred())
		istioDirNew, err := utils.DownloadIstio(istioNewVersion(), tmpDir)
		Expect(err).NotTo(HaveOccurred())

		istioctlOld := utils.GetIstioctlPath(istioDirOld)
		istioctlNew = utils.GetIstioctlPath(istioDirNew)

		By("installing Istio " + istioOldVersion())
		Expect(utils.RunIstioInstall(istioctlOld, "", "minimal")).To(Succeed())
		Expect(utils.WaitForIstioReady("")).To(Succeed())

		deployHelloWorldAndWaitForSidecar("hello-world", "istio-injection=enabled")
		deployFortsa()

		initialPod = getHelloWorldPodName("hello-world")
	})

	AfterAll(func() {
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
		if os.Getenv("SKIP_CLEANUP") == "1" {
			return
		}
		By("deleting kind cluster")
		_ = utils.KindDeleteCluster(istioClusterName(testName))
	})

	It("should restart helloworld deployment after Istio upgrade", func() {
		By("upgrading Istio to " + istioNewVersion())
		Expect(utils.RunIstioUpgrade(istioctlNew, "minimal")).To(Succeed())

		By("waiting for fortsa to annotate deployment and trigger restart")
		newPod := waitForFortsaRestart("hello-world", "helloworld", initialPod)

		By("verifying new pod has new sidecar version")
		proxyImage := getProxyImage("hello-world", newPod)
		Expect(proxyImage).To(ContainSubstring(istioNewVersion()),
			"Expected proxy image to contain %s, got: %s", istioNewVersion(), proxyImage)
	})
})

var _ = Describe("Istio revision tags", Label("Istio"), Ordered, func() {
	var (
		clusterName string
		tmpDir      string
		istioctlNew string
		initialPod  string
		testName    = "revision-tags"
	)

	BeforeAll(func() {
		skipIfIstioToolsMissing()
		clusterName = istioClusterName(testName)
		tmpDir = setupIstioCluster(clusterName)

		By("downloading Istio versions")
		istioDirOld, err := utils.DownloadIstio(istioOldVersion(), tmpDir)
		Expect(err).NotTo(HaveOccurred())
		istioDirNew, err := utils.DownloadIstio(istioNewVersion(), tmpDir)
		Expect(err).NotTo(HaveOccurred())

		istioctlOld := utils.GetIstioctlPath(istioDirOld)
		istioctlNew = utils.GetIstioctlPath(istioDirNew)

		revisionOld := utils.VersionToRevision(istioOldVersion())
		revisionNew := utils.VersionToRevision(istioNewVersion())

		By("installing Istio " + istioOldVersion() + " (revision " + revisionOld + ")")
		Expect(utils.RunIstioInstall(istioctlOld, revisionOld, "minimal")).To(Succeed())
		Expect(utils.WaitForIstioReady(revisionOld)).To(Succeed())

		By("installing Istio " + istioNewVersion() + " (revision " + revisionNew + ")")
		Expect(utils.RunIstioInstall(istioctlNew, revisionNew, "minimal")).To(Succeed())
		Expect(utils.WaitForIstioReady(revisionNew)).To(Succeed())

		By("creating revision tags")
		Expect(utils.RunIstioTagSet(istioctlNew, "stable", revisionOld, false)).To(Succeed())
		Expect(utils.RunIstioTagSet(istioctlNew, "canary", revisionNew, false)).To(Succeed())

		deployHelloWorldAndWaitForSidecar("hello-stable", "istio.io/rev=stable")
		deployHelloWorldAndWaitForSidecar("hello-canary", "istio.io/rev=canary")
		deployFortsa()

		initialPod = getHelloWorldPodName("hello-stable")
	})

	AfterAll(func() {
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
		if os.Getenv("SKIP_CLEANUP") == "1" {
			return
		}
		By("deleting kind cluster")
		_ = utils.KindDeleteCluster(istioClusterName(testName))
	})

	It("should restart helloworld deployment in hello-stable after stable tag update", func() {
		revisionNew := utils.VersionToRevision(istioNewVersion())

		By("updating stable tag to point to " + revisionNew)
		Expect(utils.RunIstioTagSet(istioctlNew, "stable", revisionNew, true)).To(Succeed())

		By("waiting for fortsa to annotate deployment and trigger restart")
		newPod := waitForFortsaRestart("hello-stable", "helloworld", initialPod)

		By("verifying new pod has canary revision proxy image")
		proxyImage := getProxyImage("hello-stable", newPod)
		Expect(proxyImage).To(ContainSubstring(istioNewVersion()),
			"Expected proxy image to contain %s, got: %s", istioNewVersion(), proxyImage)
	})
})

var _ = Describe("Istio namespace labels", Label("Istio"), Ordered, func() {
	var (
		clusterName string
		tmpDir      string
		initialPod  string
		testName    = "namespace-labels"
	)

	BeforeAll(func() {
		skipIfIstioToolsMissing()
		clusterName = istioClusterName(testName)
		tmpDir = setupIstioCluster(clusterName)

		By("downloading Istio versions")
		istioDirOld, err := utils.DownloadIstio(istioOldVersion(), tmpDir)
		Expect(err).NotTo(HaveOccurred())
		istioDirNew, err := utils.DownloadIstio(istioNewVersion(), tmpDir)
		Expect(err).NotTo(HaveOccurred())

		istioctlOld := utils.GetIstioctlPath(istioDirOld)
		istioctlNew := utils.GetIstioctlPath(istioDirNew)

		revisionOld := utils.VersionToRevision(istioOldVersion())
		revisionNew := utils.VersionToRevision(istioNewVersion())

		By("installing Istio " + istioOldVersion() + " (revision " + revisionOld + ")")
		Expect(utils.RunIstioInstall(istioctlOld, revisionOld, "minimal")).To(Succeed())
		Expect(utils.WaitForIstioReady(revisionOld)).To(Succeed())

		By("installing Istio " + istioNewVersion() + " (revision " + revisionNew + ")")
		Expect(utils.RunIstioInstall(istioctlNew, revisionNew, "minimal")).To(Succeed())
		Expect(utils.WaitForIstioReady(revisionNew)).To(Succeed())

		By("creating revision tags")
		Expect(utils.RunIstioTagSet(istioctlNew, "stable", revisionOld, false)).To(Succeed())
		Expect(utils.RunIstioTagSet(istioctlNew, "canary", revisionNew, false)).To(Succeed())

		deployHelloWorldAndWaitForSidecar("hello-ns-label", "istio.io/rev=stable")
		deployFortsa()

		initialPod = getHelloWorldPodName("hello-ns-label")
	})

	AfterAll(func() {
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
		if os.Getenv("SKIP_CLEANUP") == "1" {
			return
		}
		By("deleting kind cluster")
		_ = utils.KindDeleteCluster(istioClusterName(testName))
	})

	It("should restart helloworld deployment after namespace label change", func() {
		By("changing namespace label from istio.io/rev=stable to istio.io/rev=canary")
		cmd := exec.Command("kubectl", "label", "namespace", "hello-ns-label", "istio.io/rev=canary", "--overwrite")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for fortsa to annotate deployment and trigger restart")
		newPod := waitForFortsaRestart("hello-ns-label", "helloworld", initialPod)

		By("verifying new pod has canary revision proxy image")
		proxyImage := getProxyImage("hello-ns-label", newPod)
		Expect(proxyImage).To(ContainSubstring(istioNewVersion()),
			"Expected proxy image to contain %s, got: %s", istioNewVersion(), proxyImage)
	})
})
