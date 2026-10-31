// Package provision launches, tags, finds and terminates runner EC2 instances.
//
// Every resource the controller creates carries the tags below. Teardown and every
// terminate re-check them on the live instance, so the controller can never terminate
// an instance it did not launch.
package provision

// Tag keys.
const (
	TagName       = "Name"
	TagManaged    = "wrangl:managed"
	TagController = "wrangl:controller"
	TagRunner     = "wrangl:runner"
	TagTemplate   = "wrangl:template"
)

// Tags returns the tag set for one runner's resources.
func Tags(controllerID, runner, template string) map[string]string {
	return map[string]string{
		TagName:       "wrangl-" + runner,
		TagManaged:    "true",
		TagController: controllerID,
		TagRunner:     runner,
		TagTemplate:   template,
	}
}

// Owned reports whether tags mark a resource as managed by controllerID. An empty
// controllerID matches any wrangl-managed resource (teardown --all-controllers).
func Owned(tags map[string]string, controllerID string) bool {
	if tags[TagManaged] != "true" {
		return false
	}
	return controllerID == "" || tags[TagController] == controllerID
}
