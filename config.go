package gomiddleware

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
)

type config struct {
	BlackList []string `json:"blackList"`
	WhiteList []string `json:"whiteList"`
}

func doLog(path string, header http.Header, config config) bool {
	inBlackList := patternExists(path, config.BlackList)

	// first check if url path is in blacklist or not
	if inBlackList {
		fmt.Println(config.BlackList)
		return false
	}

	// if whitelist is not empty then result is governed by presence
	// if empty then user has not selected customised logging so proceed to log all requests
	if len(config.WhiteList) != 0 {
		inWhiteList := patternExists(path, config.WhiteList)
		return inWhiteList
	}

	return true
}

func patternExists(path string, list []string) bool {
	for _, pattern := range list {
		match, err := regexp.MatchString(pattern, path)
		if err != nil {
			log.Println("Error while pattern matching: ", err)
			log.Println("Pattern: ", pattern, " Path: ", path)
		}
		if match {
			return true
		}
	}

	return false
}

func GetConfigFromDashboard(apiUrl string) (*config, error) {
	path := apiUrl + "/middleware/config" + "?source=golang"
	resp, err := http.Get(path)
	if err != nil {
		log.Println("Error getting config from dashboard: ", err)
		return nil, err
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Println("Error while reading response body of config api response", err)
		return nil, err
	}

	config := new(config)

	error := json.Unmarshal(data, &config)
	if error != nil {
		log.Println("Error while unmarshaling response body of config api response", err)
		return nil, error
	}

	return config, nil
}
