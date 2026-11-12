package testconfig

import (
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

//go:embed config.example.json
var example []byte

type Camera struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Channel int    `json:"channel"`
}

type FixtureConfig struct {
	Username string   `json:"username"`
	Password string   `json:"password"`
	Hosts    []string `json:"hosts"`
}

type Config struct {
	Username      string        `json:"username"`
	Password      string        `json:"password"`
	ProbeCamera   string        `json:"probeCamera"`
	StorageCamera string        `json:"storageCamera"`
	Cameras       []Camera      `json:"cameras"`
	Fixtures      FixtureConfig `json:"fixtures"`
}

func Load(path string) (Config, error) {
	var config Config
	if json.Unmarshal(example, &config) != nil {
		return Config{}, errors.New("test configuration defaults are invalid")
	}
	if path == "" {
		return config, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("test configuration could not be opened")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1024*1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil {
		return Config{}, errors.New("test configuration is invalid JSON or contains unknown fields")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return Config{}, errors.New("test configuration must contain one JSON object")
	}
	return config, nil
}

func (config Config) Camera(id string) (Camera, error) {
	for _, camera := range config.Cameras {
		if camera.ID == id {
			return camera, nil
		}
	}
	return Camera{}, errors.New("selected test camera is not configured")
}

func (config Config) ValidateLive() error {
	if config.Username == "" || config.Password == "" {
		return errors.New("fill in the username and password in the ignored local test configuration")
	}
	if len(config.Cameras) < 2 {
		return errors.New("live acceptance needs at least two configured cameras")
	}
	seen := make(map[string]bool)
	for _, camera := range config.Cameras {
		address := net.ParseIP(camera.Host)
		if camera.ID == "" || seen[camera.ID] || address == nil || (!address.IsPrivate() && !address.IsLinkLocalUnicast()) || camera.Port < 1 || camera.Port > 65535 || camera.Channel < 0 {
			return errors.New("test cameras need unique IDs, private addresses, valid ports, and nonnegative channels")
		}
		seen[camera.ID] = true
	}
	if _, err := config.Camera(config.ProbeCamera); err != nil {
		return err
	}
	if _, err := config.Camera(config.StorageCamera); err != nil {
		return err
	}
	return nil
}

var fixtures = sync.OnceValue(func() FixtureConfig {
	config, err := Load(os.Getenv("REONVR_TEST_CONFIG"))
	if err != nil || len(config.Fixtures.Hosts) < 6 {
		panic("test fixture configuration is missing or invalid")
	}
	if config.Fixtures.Username == "" {
		config.Fixtures.Username = rand.Text()
	}
	if config.Fixtures.Password == "" {
		config.Fixtures.Password = rand.Text()
	}
	return config.Fixtures
})

func Host(index int) string {
	return fixtures().Hosts[index-1]
}

func Username() string {
	return fixtures().Username
}

func Password() string {
	return fixtures().Password
}

func SpecialPassword() string {
	return Password() + string([]byte{' ', '/', ':', '@'})
}

func Expand(value string) string {
	replacements := []string{"{{username}}", Username(), "{{password}}", Password(), "{{specialPassword}}", SpecialPassword()}
	for index, host := range fixtures().Hosts {
		replacements = append(replacements, "{{host"+strconv.Itoa(index+1)+"}}", host)
	}
	return strings.NewReplacer(replacements...).Replace(value)
}
