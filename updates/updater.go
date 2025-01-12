package updates

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Safing/portbase/log"
)

func updater() {
	time.Sleep(10 * time.Second)
	for {
		err := CheckForUpdates()
		if err != nil {
			log.Warningf("updates: failed to check for updates: %s", err)
		}
		time.Sleep(1 * time.Hour)
	}
}

func CheckForUpdates() error {

	// ensure core components are updated
	var err error
	if runtime.GOOS == "windows" {
		_, err = GetPlatformFile("pmctl/pmctl.exe")
	} else {
		_, err = GetPlatformFile("pmctl/pmctl")
	}
	if err != nil {
		log.Errorf("updates: failed to mark pmctl/pmctl as used to ensure updates: %s", err)
	}

	// download new index
	var data []byte
	for tries := 0; tries < 3; tries++ {
		data, err = fetchData("stable.json", tries)
		if err == nil {
			break
		}
	}
	if err != nil {
		return err
	}

	newStableUpdates := make(map[string]string)
	err = json.Unmarshal(data, &newStableUpdates)
	if err != nil {
		return err
	}

	if len(newStableUpdates) == 0 {
		return errors.New("stable.json is empty")
	}

	// FIXME IN STABLE: correct log line
	log.Infof("updates: downloaded new update index: stable.json (alpha until we actually reach stable)")

	// update existing files
	log.Tracef("updates: updating existing files")
	updatesLock.RLock()
	for identifier, newVersion := range newStableUpdates {
		oldVersion, ok := localUpdates[identifier]
		if ok && newVersion != oldVersion {

			filePath := GetVersionedPath(identifier, newVersion)
			realFilePath := filepath.Join(updateStoragePath, filePath)
			for tries := 0; tries < 3; tries++ {
				err := fetchFile(realFilePath, filePath, tries)
				if err == nil {
					break
				}
			}
			if err != nil {
				log.Warningf("failed to update %s to %s: %s", identifier, newVersion, err)
			}

		}
	}
	updatesLock.RUnlock()
	log.Tracef("updates: finished updating existing files")

	// update stable index
	updatesLock.Lock()
	stableUpdates = newStableUpdates
	updatesLock.Unlock()

	// save stable index
	err = ioutil.WriteFile(filepath.Join(updateStoragePath, "stable.json"), data, 0644)
	if err != nil {
		log.Warningf("updates: failed to save new version of stable.json: %s", err)
	}

	// update version status
	updatesLock.RLock()
	defer updatesLock.RUnlock()
	updateStatus(versionClassStable, stableUpdates)

	return nil
}
