package runtime

// PodImage is what a read of an issue pod's image answers. The idle release reads it before it
// releases a pod (the Sandbox runtime's IssuePodImage, workflow.IdleRelease). A pod on the
// configured image and a pod on another image are both released once they are idle: a pod on
// another image keeps working on it until then, and its next pod takes the configured image. A pod
// whose image the read could not see is held, since nothing says which pod the release would take.
type PodImage int

const (
	// PodImageUnread: the read saw no image. There is no Sandbox, no pod, a foreign pod or an
	// ended pod, no Sandbox name can be built from the key, or the store read failed. It is the
	// zero value on purpose: an answer nobody set holds the pod.
	PodImageUnread PodImage = iota
	// PodImageConfigured: every container of the pod, init containers included, runs the image
	// the daemon's configuration names.
	PodImageConfigured
	// PodImageOther: a container of the pod runs another image than the one the daemon's
	// configuration names.
	PodImageOther
)
