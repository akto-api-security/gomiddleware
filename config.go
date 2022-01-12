package gomiddleware

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"
)

type config struct {
	BlackList []string `json:"blackList"`
	WhiteList []string `json:"whiteList"`
}

func doLogBasedOnPath(path string, header http.Header, config config) bool {
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

func doLogBasedOnResponseHeader(header http.Header) bool {
	content_type := header.Get("Content-Type")

	return strings.Contains(content_type, "application/json")

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
	config := new(config)
	path := apiUrl + "/middleware/config" + "?source=golang"
	resp, err := http.Get(path)
	if err != nil {
		log.Println("Error getting config from dashboard: ", err)
		return nil, err
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println("Error while reading response body of config api response", err)
		return config, err
	}

	err = json.Unmarshal(data, &config)
	if err != nil {
		log.Println("Error while unmarshaling response body of config api response", err)
		return config, err
	}

	return config, nil
}
