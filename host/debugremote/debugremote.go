package debugremote

const Label = "dev.fevom.debug"

const (
	LabelHelper  = "helper"
	LabelSession = "session"
	LabelStore   = "store"
	LabelTemp    = "temp"
)

const StoreVolume = "fev-debug-store"

func StoreVolumeFor(stamp string) string {
	if stamp == "" {
		return StoreVolume
	}
	return StoreVolume + "-" + stamp
}

const HelperRepo = "fev-debug-helper"
