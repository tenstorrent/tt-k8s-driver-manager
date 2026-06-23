package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	firmwarev1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/firmware/v1alpha1"
)

// jobName returns a stable, DNS-safe Job name for a (CR, node, version) triple.
// Length-capped at 63 chars; suffix is a short hash for uniqueness when the
// readable prefix gets truncated.
func jobName(cr *firmwarev1alpha1.TenstorrentFirmwarePolicy, nodeName, version string) string {
	prefix := fmt.Sprintf("ttfwp-%s-%s", cr.Name, nodeName)
	if len(prefix) > 48 {
		prefix = prefix[:48]
	}
	h := sha256.Sum256([]byte(cr.Name + "/" + nodeName + "/" + version))
	suffix := hex.EncodeToString(h[:4])
	return strings.ToLower(fmt.Sprintf("%s-%s-%s", prefix, sanitizeVersion(version), suffix))
}

func sanitizeVersion(v string) string {
	return strings.ReplaceAll(v, ".", "-")
}

func boolEnv(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// buildFlashJob templates the per-node flash Job. The bundle is fetched by an
// initContainer into a shared emptyDir; the flasher container runs tt-flash
// then asserts readback via tt-smi.
func buildFlashJob(cr *firmwarev1alpha1.TenstorrentFirmwarePolicy, nodeName, defaultImage string) *batchv1.Job {
	version := cr.Spec.Version
	readback := cr.Spec.ReadbackVersion
	if readback == "" {
		readback = version + ".0"
	}
	bundleURL := cr.Spec.BundleURL
	if bundleURL == "" {
		bundleURL = fmt.Sprintf(
			"https://github.com/tenstorrent/tt-system-firmware/releases/download/v%s/fw_pack-%s.fwbundle",
			version, version,
		)
	}

	image := defaultImage
	pullPolicy := corev1.PullIfNotPresent
	forceWrite := false
	continueOnReadbackFailure := false
	if cr.Spec.Flasher != nil {
		if cr.Spec.Flasher.Image != "" {
			image = cr.Spec.Flasher.Image
		}
		if cr.Spec.Flasher.ImagePullPolicy != "" {
			pullPolicy = cr.Spec.Flasher.ImagePullPolicy
		}
		forceWrite = cr.Spec.Flasher.ForceWrite
		continueOnReadbackFailure = cr.Spec.Flasher.ContinueOnReadbackFailure
	}

	// ForceWrite bypasses both the script-level "already at target" skip and
	// tt-flash's own version-match check. ContinueOnReadbackFailure independently
	// lets the script proceed when tt-smi can't read the chip.
	flashArgs := ""
	if forceWrite {
		flashArgs = "--force"
	}

	timeout := int64(cr.Spec.UpgradePolicy.FlashTimeoutSeconds)
	if timeout == 0 {
		timeout = 900
	}
	backoffLimit := int32(0)
	ttl := int32(86400)
	privileged := true

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(cr, nodeName, version),
			Namespace: operatorNamespace(),
			Labels: map[string]string{
				JobLabelCR:      cr.Name,
				JobLabelNode:    nodeName,
				JobLabelVersion: sanitizeVersion(version),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			ActiveDeadlineSeconds:   &timeout,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						JobLabelCR:      cr.Name,
						JobLabelNode:    nodeName,
						JobLabelVersion: sanitizeVersion(version),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					HostNetwork:   true,
					NodeName:      nodeName,
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					InitContainers: []corev1.Container{
						{
							Name: "fetch-bundle",
							// Pin: floating tags lock us into "latest at build time
							// of the controller" semantics, which surprises people.
							Image: "curlimages/curl:8.10.1",
							Command: []string{"sh", "-c", `curl -fsSL "$TT_FW_BUNDLE_URL" -o /work/bundle.fwbundle`},
							Env: []corev1.EnvVar{
								{Name: "TT_FW_BUNDLE_URL", Value: bundleURL},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "work", MountPath: "/work"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "flash",
							Image:           image,
							ImagePullPolicy: pullPolicy,
							// Consolidated tools image: ENTRYPOINT is the
							// dispatcher; first arg selects the role.
							Args: []string{"flash"},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_RAWIO", "SYS_ADMIN"},
								},
							},
							Env: []corev1.EnvVar{
								{Name: "TT_FW_BUNDLE_PATH", Value: "/work/bundle.fwbundle"},
								{Name: "TT_FW_VERSION", Value: version},
								{Name: "TT_FW_READBACK", Value: readback},
								{Name: "TT_FLASH_ARGS", Value: flashArgs},
								{Name: "TT_FORCE_WRITE", Value: boolEnv(forceWrite)},
								{Name: "TT_CONTINUE_ON_READBACK_FAILURE", Value: boolEnv(continueOnReadbackFailure)},
							},
							// /dev/tenstorrent comes from privileged's auto-mounted /dev
							// (containerd bind-mounts host /dev into privileged containers).
							// An explicit hostPath here overlays a stale per-subpath bind
							// that doesn't follow host-side destroy/recreate.
							VolumeMounts: []corev1.VolumeMount{
								{Name: "work", MountPath: "/work", ReadOnly: true},
								{Name: "hugepages", MountPath: "/dev/hugepages"},
								{Name: "sys", MountPath: "/sys"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "hugepages", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/hugepages"}}},
						{Name: "sys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys"}}},
					},
				},
			},
		},
	}
}

// jobFinished returns (completed, succeeded). If !completed, both bools are false.
func jobFinished(job *batchv1.Job) (bool, bool) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete, batchv1.JobSuccessCriteriaMet:
			return true, true
		case batchv1.JobFailed:
			return true, false
		}
	}
	return false, false
}

// setOwnerRef attaches the CR as owner so Job GC follows CR deletion.
func setOwnerRef(obj client.Object, cr *firmwarev1alpha1.TenstorrentFirmwarePolicy) {
	gvk := firmwarev1alpha1.GroupVersion.WithKind("TenstorrentFirmwarePolicy")
	controller := true
	blockOwnerDeletion := true
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion:         gvk.GroupVersion().String(),
		Kind:               gvk.Kind,
		Name:               cr.Name,
		UID:                cr.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}})
}
