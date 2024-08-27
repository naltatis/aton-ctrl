package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stianeikeland/go-rpio/v4"
)

const (
	V       float64 = 230
	min_PWM float64 = 10
	max_PWM float64 = 92
	start_W float64 = 45
	min_W   float64 = 0
	max_W   float64 = 3000
	min_A   float64 = 0
	max_A   float64 = max_W / V // 13,044 A
	start_A float64 = start_W / V
)

var enableOffset float64 = 0
var maxTemp float64 = 0

var mu sync.Mutex
var requestedCurrent float64 = 0
var enabled bool = false
var temp float64 = 0
var tempOutdated bool = true
var heating bool = false
var pwm float64 = min_PWM
var pin rpio.Pin

type Config struct {
	EnableOffset float64 `json:"enableOffset"`
	MaxTemp      float64 `json:"maxTemp"`
}

func main() {
	file, err := os.ReadFile("/home/pi/aton.json")
	if err != nil {
		fmt.Printf("Error reading config file: %s\n", err)
		os.Exit(1)
	}

	var config Config
	err = json.Unmarshal(file, &config)
	if err != nil {
		fmt.Printf("Error parsing config file: %s\n", err)
		os.Exit(1)
	}
	maxTemp = config.MaxTemp
	enableOffset = config.EnableOffset

	err = rpio.Open()
	if err == nil {
		defer rpio.Close()

		pin = rpio.Pin(12) // PWM0
		pin.Mode(rpio.Pwm)
		pin.Freq(1000 * 1000)
	}

	go func() {
		for {
			// read temperature
			checkTemp()

			// check if heating should be started or stopped
			heating = shouldHeat()

			// determine needed current
			current := 0.0
			if heating {
				current = requestedCurrent
			}

			// send signal to heater
			pwm = currentToPwm(current)
			if err == nil {
				fmt.Printf("pwm: %.2f, current: %.2f, temp: %.2f, heating: %t\n", pwm, current, temp, heating)
				pin.DutyCycleWithPwmMode(uint32(pwm*10), 1000, rpio.MarkSpace)
			}
			time.Sleep(time.Second * 10)
		}
	}()

	http.HandleFunc("/current", handleCurrent)
	http.HandleFunc("/enable", handleEnable)
	http.HandleFunc("/state", handleState)
	http.HandleFunc("/maxtemp", handleMaxTemp)
	http.ListenAndServe(":3000", nil)
}

type UvrData struct {
	UpdatedAt  int64   `json:"updated_at"`
	PufferOben float64 `json:"puffer_oben"`
}

func checkTemp() {
	tempFile := "/home/pi/temp.txt"
	content, err := os.ReadFile(tempFile)
	if err != nil {
		fmt.Println("unable to read temp.txt: temp outdated")
		tempOutdated = true
		return
	}

	fileInfo, err := os.Stat(tempFile)
	if err != nil {
		fmt.Println("unable to stat temp.txt: temp outdated")
		tempOutdated = true
		return
	}
	updateTimestamp := fileInfo.ModTime().UnixNano()
	updateTime := time.Unix(0, int64(updateTimestamp))
	tempOutdated = time.Since(updateTime).Minutes() > 5
	temp, err = strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
	if err != nil {
		fmt.Println("unable to parse temp.txt: temp outdated")
		tempOutdated = true
		return
	}
}

// check of heating should be started or stopped
func shouldHeat() bool {
	if heating && tempOutdated {
		fmt.Println("heating disabled: temp outdated")
		return false
	}

	if heating && !enabled {
		fmt.Println("heating disabled: disabled by api")
		return false
	}

	if !heating && enabled && !tempOutdated && temp < maxTemp-enableOffset {
		fmt.Println("heating enabled")
		return true
	}

	if heating && temp > maxTemp {
		fmt.Println("heating disabled: max temp reached")
		return false
	}

	// no change required
	return heating
}

// 10-90% PWM entsprechen 0-3000W Leistung, der Betrieb beginnt aber erst bei mind. 45W (entspricht etwa 12% PWM).
// https://www.ta.co.at/download/datei/34523507-manual-aton-montage-bedienung/
func currentToPwm(input float64) float64 {
	if input <= start_A {
		return min_PWM
	}

	if input > max_A {
		return max_PWM
	}

	return min_PWM + ((max_PWM-min_PWM)/(max_A-min_A))*(input-min_A)
}

func pwmToWatt(input float64) int64 {
	watt := min_W + ((max_W-min_W)/(max_PWM-min_PWM))*(input-min_PWM)
	if watt < start_W {
		return 0
	}
	return int64(watt)
}

// POST /current 0-13
func handleCurrent(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}
	defer req.Body.Close()

	current, err := strconv.ParseFloat(strings.TrimSpace(string(body)), 64)
	if err != nil {
		fmt.Println("body:", string(body))
		http.Error(w, "Error parsing float value", http.StatusBadRequest)
		return
	}

	mu.Lock()
	requestedCurrent = current
	mu.Unlock()

	fmt.Printf("API: requested current: %.2f\n", current)

	// Send a response
	w.WriteHeader(http.StatusOK)
}

// POST /maxtemp 0-100
func handleMaxTemp(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}
	defer req.Body.Close()

	temp, err := strconv.ParseFloat(strings.TrimSpace(string(body)), 64)
	if err != nil {
		fmt.Println("body:", string(body))
		http.Error(w, "Error parsing float value", http.StatusBadRequest)
		return
	}

	mu.Lock()
	maxTemp = temp
	mu.Unlock()

	fmt.Printf("API: max temp: %.2f\n", maxTemp)

	// Send a response
	w.WriteHeader(http.StatusOK)
}

// POST /enable "true|false"
func handleEnable(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}
	defer req.Body.Close()

	enable, err := strconv.ParseBool(strings.TrimSpace(string(body)))
	if err != nil {
		fmt.Println("body:", string(body))
		http.Error(w, "Error parsing boolean value", http.StatusBadRequest)
		return
	}

	mu.Lock()
	enabled = enable
	mu.Unlock()

	fmt.Printf("API: enabled: %t\n", enable)

	// Send a response
	w.WriteHeader(http.StatusOK)
}

type StateResponse struct {
	Enabled          bool    `json:"enabled"`
	RequestedCurrent float64 `json:"current"`
	Watt             int64   `json:"watt"`
	PWM              float64 `json:"pwm"`
	Temp             float64 `json:"temp"`
	EnableOffset     float64 `json:"enableOffset"`
	MaxTemp          float64 `json:"maxTemp"`
	Heating          bool    `json:"heating"`
	Status           string  `json:"status"`
}

// GET /state > JSON
func handleState(w http.ResponseWriter, req *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	status := "B" // connected
	if tempOutdated {
		status = "F" // error
	} else if heating {
		status = "C" // charging aka heating
	}

	result := StateResponse{
		Enabled:          enabled,
		RequestedCurrent: requestedCurrent,
		PWM:              pwm,
		Watt:             pwmToWatt(pwm),
		Temp:             temp,
		MaxTemp:          maxTemp,
		EnableOffset:     enableOffset,
		Heating:          heating,
		Status:           status,
	}

	jsonData, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintf(w, "Error: %s", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonData)
}
