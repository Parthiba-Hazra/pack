package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/buildpacks/pack/pkg/logging"
	"github.com/pkg/errors"
)

// PullPolicy defines a policy for how to manage images
type PullPolicy int

var interval string

var (
	intervalRegex = regexp.MustCompile(`^(\d+d)?(\d+h)?(\d+m)?$`)
	imagePath     string
)

const (
	// PullAlways images, even if they are present
	PullAlways PullPolicy = iota
	// PullNever images, even if they are not present
	PullNever
	// PullIfNotPresent pulls images if they aren't present
	PullIfNotPresent
	// PullWithInterval pulls images with specified intervals
	PullWithInterval
)

type ImageJSON struct {
	Interval struct {
		PullingInterval  string `json:"pulling_interval"`
		PruningIinterval string `json:"pruning_interval"`
		LastPrune        string `json:"last_prune"`
	} `json:"interval"`
	Image struct {
		ImageIDtoTIME map[string]string
	} `json:"image"`
}

var nameMap = map[string]PullPolicy{"always": PullAlways, "never": PullNever, "if-not-present": PullIfNotPresent, "": PullAlways}

// ParsePullPolicy from string with support for interval formats
func ParsePullPolicy(policy string) (PullPolicy, error) {
	if val, ok := nameMap[policy]; ok {
		return val, nil
	}

	if strings.HasPrefix(policy, "interval=") {
		interval = policy
		intervalStr := strings.TrimPrefix(policy, "interval=")
		matches := intervalRegex.FindStringSubmatch(intervalStr)
		if len(matches) == 0 {
			return PullAlways, errors.Errorf("invalid interval format: %s", intervalStr)
		}

		updateImageJSONDuration(intervalStr)

		return PullWithInterval, nil
	}

	return PullAlways, errors.Errorf("invalid pull policy %s", policy)
}

func (p PullPolicy) String() string {
	switch p {
	case PullAlways:
		return "always"
	case PullNever:
		return "never"
	case PullIfNotPresent:
		return "if-not-present"
	case PullWithInterval:
		return fmt.Sprintf("interval=%v", interval)
	}

	return ""
}

func updateImageJSONDuration(intervalStr string) error {
	imageJSON, err := readImageJSON(logging.NewSimpleLogger(os.Stderr))
	if err != nil {
		return err
	}

	imageJSON.Interval.PullingInterval = intervalStr

	updatedJSON, err := json.MarshalIndent(imageJSON, "", "    ")
	if err != nil {
		return errors.Wrap(err, "failed to marshal updated records")
	}

	return os.WriteFile(imagePath, updatedJSON, 0644)
}

func readImageJSON(l logging.Logger) (ImageJSON, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ImageJSON{}, errors.Wrap(err, "failed to get home directory")
	}
	imagePath = filepath.Join(homeDir, ".pack", "image.json")

	// Check if the directory exists, if not, create it
	dirPath := filepath.Dir(imagePath)
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		l.Warnf("missing `.pack` directory under %s directory %s", homeDir, err)
		l.Debugf("creating `.pack` directory under %s directory", homeDir)
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			return ImageJSON{}, errors.Wrap(err, "failed to create directory")
		}
	}

	// Check if the file exists, if not, create it with minimum JSON
	if _, err := os.Stat(imagePath); os.IsNotExist(err) {
		l.Warnf("missing `image.json` file under %s directory %s", dirPath, err)
		l.Debugf("creating `image.json` file under %s directory", dirPath)
		minimumJSON := []byte(`{"interval":{"pulling_interval":"","pruning_interval":"7d","last_prune":""},"image":{}}`)
		if err := os.WriteFile(imagePath, minimumJSON, 0644); err != nil {
			return ImageJSON{}, errors.Wrap(err, "failed to create image.json file")
		}
	}

	jsonData, err := os.ReadFile(imagePath)
	if err != nil && !os.IsNotExist(err) {
		return ImageJSON{}, errors.Wrap(err, "failed to read image.json")
	}

	var imageJSON ImageJSON
	if err := json.Unmarshal(jsonData, &imageJSON); err != nil && !os.IsNotExist(err) {
		return ImageJSON{}, errors.Wrap(err, "failed to unmarshal image.json")
	}

	return imageJSON, nil
}

func (f *Fetcher) CheckImagePullInterval(imageID string) (bool, error) {
	imageJSON, err := readImageJSON(f.logger)
	if err != nil {
		return false, err
	}

	timestamp, ok := imageJSON.Image.ImageIDtoTIME[imageID]
	if !ok {
		// If the image ID is not present, return true
		return true, nil
	}

	imageTimestamp, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return false, errors.Wrap(err, "failed to parse image timestamp from JSON")
	}

	durationStr := imageJSON.Interval.PullingInterval

	duration, err := parseDurationString(durationStr)
	if err != nil {
		return false, errors.Wrap(err, "failed to parse duration from JSON")
	}

	timeThreshold := time.Now().Add(-duration)

	return imageTimestamp.Before(timeThreshold), nil
}

func parseDurationString(durationStr string) (time.Duration, error) {
	var totalMinutes int
	for i := 0; i < len(durationStr); {
		endIndex := i + 1
		for endIndex < len(durationStr) && durationStr[endIndex] >= '0' && durationStr[endIndex] <= '9' {
			endIndex++
		}

		value, err := strconv.Atoi(durationStr[i:endIndex])
		if err != nil {
			return 0, errors.Wrapf(err, "invalid interval format: %s", durationStr)
		}
		unit := durationStr[endIndex]

		switch unit {
		case 'd':
			totalMinutes += value * 24 * 60
		case 'h':
			totalMinutes += value * 60
		case 'm':
			totalMinutes += value
		default:
			return 0, errors.Errorf("invalid interval uniit: %s", string(unit))
		}

		i = endIndex + 1
	}

	return time.Duration(totalMinutes) * time.Minute, nil
}

func (f *Fetcher) PruneOldImages() error {
	imageJSON, err := readImageJSON(f.logger)
	if err != nil {
		return err
	}

	if imageJSON.Interval.LastPrune != "" {
		lastPruneTime, err := time.Parse(time.RFC3339, imageJSON.Interval.LastPrune)
		if err != nil {
			return errors.Wrap(err, "failed to parse last prune timestamp from JSON")
		}

		pruningInterval, err := parseDurationString(imageJSON.Interval.PruningIinterval)
		if err != nil {
			return errors.Wrap(err, "failed to parse pruning interval from JSON")
		}

		if time.Since(lastPruneTime) < pruningInterval {
			// not enough time has passed since the last prune
			return nil
		}
	}

	// prune images older than the pruning interval
	pruningDuration, err := parseDurationString(imageJSON.Interval.PruningIinterval)
	if err != nil {
		return errors.Wrap(err, "failed to parse pruning interval from JSON")
	}

	pruningThreshold := time.Now().Add(-pruningDuration)

	for imageID, timestamp := range imageJSON.Image.ImageIDtoTIME {
		imageTimestamp, err := time.Parse(time.RFC3339, timestamp)
		if err != nil {
			return errors.Wrap(err, "failed to parse image timestamp fron JSON")
		}

		if imageTimestamp.Before(pruningThreshold) {
			delete(imageJSON.Image.ImageIDtoTIME, imageID)
		}
	}

	imageJSON.Interval.LastPrune = time.Now().Format(time.RFC3339)

	updatedJSON, err := json.MarshalIndent(imageJSON, "", "    ")
	if err != nil {
		return errors.Wrap(err, "failed to marshal updated records")
	}

	if err := os.WriteFile(imagePath, updatedJSON, 0644); err != nil {
		return errors.Wrap(err, "failed to write updated image.json")
	}

	return nil
}
