package device

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mavsphere/mavsphere-agent-go/pkg/config"
)

// DetectVideoDevice tries to find a compatible video capture device supporting MJPG, H264, or YUYV.
// It checks both that the device reports compatible pixel formats AND that it is a V4L2 capture
// device (not an output-only node such as bcm2835-codec or a loopback device before it is fed).
// Hardware codec nodes on the Raspberry Pi (bcm2835-codec) report H264 formats but have
// V4L2_CAP_VIDEO_OUTPUT capability, not V4L2_CAP_VIDEO_CAPTURE — this check rejects them.
func DetectVideoDevice() (string, error) {
	matches, err := filepath.Glob("/dev/video*")
	if err != nil {
		return "", err
	}

	for _, device := range matches {
		info, err := os.Stat(device)
		if err != nil || info.IsDir() {
			continue
		}

		// First check: must be a capture device (not a codec output or loopback output node).
		// v4l2-ctl --info prints "Video Capture" in Device Caps if V4L2_CAP_VIDEO_CAPTURE is set.
		infoCmd := exec.Command("v4l2-ctl", "--info", "-d", device)
		var infoOut bytes.Buffer
		infoCmd.Stdout = &infoOut
		infoCmd.Stderr = nil
		if err := infoCmd.Run(); err != nil {
			continue
		}
		if !strings.Contains(infoOut.String(), "Video Capture") {
			continue // output-only or codec node — skip
		}

		// Second check: must report a pixel format GStreamer can use as a source.
		fmtCmd := exec.Command("v4l2-ctl", "--list-formats", "-d", device)
		var fmtOut bytes.Buffer
		fmtCmd.Stdout = &fmtOut
		fmtCmd.Stderr = nil
		if err := fmtCmd.Run(); err != nil {
			continue
		}

		output := fmtOut.String()
		if strings.Contains(output, "H264") || strings.Contains(output, "MJPG") || strings.Contains(output, "YUYV") {
			fmt.Printf("[Video] ✅ Found compatible capture device: %s\n", device)
			return device, nil
		}
	}

	return "", fmt.Errorf("[Video] ❌ No compatible video capture device found")
}

func DetectAudioDevice() (string, error) {
	cmd := exec.Command("arecord", "-l")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("arecord -l failed: %w", err)
	}

	lines := strings.Split(out.String(), "\n")
	var card, dev string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Example:
		// card 1: USB [USB Audio Device], device 0: USB Audio [USB Audio]
		if strings.HasPrefix(line, "card ") {
			// find "card N:" and "device M:"
			fields := strings.Fields(line)
			// fields: ["card", "1:", "USB", "[USB", "Audio", "Device],", "device", "0:", ...]
			for i := 0; i < len(fields); i++ {
				// card index
				if fields[i] == "card" && i+1 < len(fields) {
					cardField := strings.TrimSuffix(fields[i+1], ":")
					card = cardField
				}
				// device index
				if fields[i] == "device" && i+1 < len(fields) {
					devField := strings.TrimSuffix(fields[i+1], ":")
					dev = devField
				}
			}
			if card != "" && dev != "" {
				break
			}
		}
	}

	if card == "" || dev == "" {
		return "", fmt.Errorf("no ALSA capture card/device found in arecord -l output")
	}

	device := fmt.Sprintf("hw:%s,%s", card, dev)
	return device, nil
}

func DetectCapabilities(cfg *config.AgentConfig, videoDevice string, audioDevice string, controlEnabled bool, isAircraftLike bool) []string {
	caps := map[string]bool{}

	if cfg.MavlinkConnection != "" {
		caps["TELEMETRY"] = true
		if controlEnabled {
			caps["CONTROL"] = true
		}
	}
	if videoDevice != "" {
		caps["VIDEO_FPV"] = true
	}
	if audioDevice != "" {
		caps["AUDIO_MAV"] = true
	}

	// Thrust profile caps (used by UI to decide center-split vs forward-only)
	// Always advertise so UI can render correctly even before control is enabled.
	if strings.EqualFold(cfg.ThrustMode, "THRUST_FWD_ONLY") {
		caps["THRUST_FWD_ONLY"] = true
	} else {
		caps["THRUST_FWD_REV"] = true
	}

	// Control feature flags
	if cfg.AllowGotoEffective() {
		caps["GOTO"] = true
	}
	if cfg.AllowRcOverrideEffective() {
		caps["RC_OVERRIDE"] = true
	}

	var list []string
	for k := range caps {
		list = append(list, k)
	}
	slices.Sort(list)
	return list
}
