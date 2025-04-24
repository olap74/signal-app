package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/faiface/beep/mp3"
	"github.com/faiface/beep/speaker"
)

type Config struct {
	APIURL             string            `json:"api_url"`
	AuthHeader         string            `json:"auth_header"`
	AudioFiles         map[string]string `json:"audio_files"`
	AlertOnEmpty       string            `json:"alert_on_empty"`
	Debug              bool              `json:"debug"`
	LogToFile          bool              `json:"log_to_file"`
	LogFilePath        string            `json:"log_file_path"`
	TimeZone           string            `json:"time_zone"`
	RepeatAudioFile    string            `json:"repeat_audio_file"`
	RepeatIntervalMin  int               `json:"repeat_interval_min"`
	RequestIntervalSec int               `json:"request_interval_sec"`
	EnableRepeatAudio  bool              `json:"enable_repeat_audio"` // Додано поле для керування повторюваним сигналом
}

type Region struct {
	LastUpdate   string  `json:"lastUpdate"`
	ActiveAlerts []Alert `json:"activeAlerts"`
}

type Alert struct {
	Type       string `json:"type"`
	LastUpdate string `json:"lastUpdate"`
}

type State struct {
	ActiveAlertTypes map[string]bool      `json:"active_alert_types"`
	LastUpdate       string               `json:"last_update"`
	LastPlayed       map[string]time.Time `json:"last_played"`
}

func main() {
	// Розбір прапорців
	configPath := flag.String("config", "config.json", "Шлях до файлу налаштувань")
	statePath := flag.String("state", "state.json", "Шлях до файлу стану")
	help := flag.Bool("help", false, "Вивести інформацію про налаштування та вийти")
	configDesc := flag.Bool("config-desc", false, "Вивести опис файлу конфігурації та вийти")
	flag.Parse()

	// Якщо вказано прапорець help, виводимо інформацію про налаштування
	if *help {
		fmt.Println("Програма для моніторингу подій та відтворення аудіо.")
		fmt.Println("Доступні прапорці:")
		flag.PrintDefaults()
		return
	}

	// Якщо вказано прапорець config-desc, виводимо опис файлу конфігурації
	if *configDesc {
		printConfigDescription()
		return
	}

	// Завантажуємо конфігурацію
	config, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Помилка завантаження конфігурації: %v", err)
	}

	// Налаштовуємо логування
	setupLogging(config)

	// Завантажуємо попередній стан
	state, err := loadState(*statePath)
	if err != nil {
		log.Printf("Не вдалося завантажити попередній стан: %v", err)
		state = &State{
			ActiveAlertTypes: make(map[string]bool),
			LastUpdate:       "",
			LastPlayed:       make(map[string]time.Time),
		}
	}

	// Синхронізація часу з сервером
	_, lastUpdate, err := fetchAlerts(config) // Прибираємо змінну alerts
	if err != nil {
		log.Fatalf("Помилка отримання даних під час запуску: %v", err)
	}
	log.Printf("Синхронізація: час з сервера: %s, час у state.json: %s", lastUpdate, state.LastUpdate)

	if state.LastUpdate != lastUpdate {
		log.Printf("Оновлюємо час у state.json: %s -> %s", state.LastUpdate, lastUpdate)
		state.LastUpdate = lastUpdate
		saveState(state, *statePath)
	}

	// Перевіряємо, що час у state.json синхронізовано
	if state.LastUpdate != lastUpdate {
		log.Fatalf("Помилка синхронізації часу: час у state.json (%s) не збігається з часом сервера (%s)", state.LastUpdate, lastUpdate)
	}

	// Визначаємо локальну часову зону
	location, err := time.LoadLocation(config.TimeZone)
	if err != nil {
		log.Fatalf("Помилка завантаження часової зони: %v", err)
	}

	// Основна логіка програми
	runMainLoop(config, state, location, *statePath)
}

func runMainLoop(config *Config, state *State, location *time.Location, statePath string) {
	// Встановлюємо інтервал запитів до сервера
	requestInterval := time.Duration(config.RequestIntervalSec) * time.Second
	if config.RequestIntervalSec <= 0 {
		requestInterval = 30 * time.Second // Значення за замовчуванням
	}

	// Основний цикл
	for {
		// Крок 1: Запит на отримання даних з сервера
		alerts, lastUpdate, err := fetchAlerts(config)
		if err != nil {
			log.Printf("Помилка отримання даних: %v", err)
			time.Sleep(requestInterval)
			continue
		}

		log.Printf("Час з сервера (UTC): %s", lastUpdate)

		// Крок 2: Порівняння часу останнього оновлення
		if state.LastUpdate != lastUpdate {
			log.Printf("Оновлюємо час у state.json: %s -> %s", state.LastUpdate, lastUpdate)
			state.LastUpdate = lastUpdate
			saveState(state, statePath)
		}

		// Крок 3: Перевірка, чи активна подія
		currentAlerts := make(map[string]bool)
		for _, alert := range alerts {
			currentAlerts[alert.Type] = true
		}

		checkAndHandleStateChange(state, currentAlerts, alerts, lastUpdate, config, statePath)

		// Крок 4: Перевірка необхідності відтворення звуку
		checkAndPlayRepeatAudio(state, config, location, statePath)

		time.Sleep(requestInterval)
	}
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) // Заміщено ioutil.ReadFile на os.ReadFile
	if err != nil {
		return nil, err
	}

	// Видаляємо коментарі з JSON
	cleanedData := removeComments(data)

	var config Config
	err = json.Unmarshal(cleanedData, &config)
	return &config, err
}

func removeComments(data []byte) []byte {
	var buffer bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(data))
	commentRegex := regexp.MustCompile(`^\s*(//|#)`)

	for scanner.Scan() {
		line := scanner.Text()
		if !commentRegex.MatchString(line) {
			buffer.WriteString(line + "\n")
		}
	}

	return buffer.Bytes()
}

func fetchAlerts(config *Config) ([]Alert, string, error) {
	req, err := http.NewRequest("GET", config.APIURL, nil)
	if err != nil {
		return nil, "", err
	}

	// Встановлюємо заголовок авторизації
	req.Header.Set("Authorization", config.AuthHeader)

	if config.Debug {
		log.Printf("Відправка запиту: %s", config.APIURL)
		// log.Printf("Заголовок Authorization: %s", config.AuthHeader) // Прибрано з логів
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if config.Debug {
		log.Printf("Отримано відповідь: %d", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("неочікуваний статус відповіді: %d", resp.StatusCode)
	}

	var regions []Region
	err = json.NewDecoder(resp.Body).Decode(&regions)
	if err != nil {
		return nil, "", err
	}

	if len(regions) > 0 {
		region := regions[0]
		if len(region.ActiveAlerts) > 0 {
			return region.ActiveAlerts, region.ActiveAlerts[0].LastUpdate, nil
		}
		return nil, region.LastUpdate, nil
	}
	return nil, "", nil
}

func playAudio(path string) {
	if path == "" {
		log.Println("Аудіофайл не вказано")
		return
	}

	f, err := os.Open(path)
	if err != nil {
		log.Printf("Помилка відкриття аудіофайлу: %v", err)
		return
	}
	defer f.Close()

	streamer, format, err := mp3.Decode(f)
	if err != nil {
		log.Printf("Помилка декодування аудіофайлу: %v", err)
		return
	}
	defer streamer.Close()

	speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10))
	speaker.Play(streamer)
	select {
	case <-time.After(format.SampleRate.D(streamer.Len())):
	}
}

func loadState(path string) (*State, error) {
	data, err := os.ReadFile(path) // Заміщено ioutil.ReadFile на os.ReadFile
	if err != nil {
		if os.IsNotExist(err) {
			return &State{ActiveAlertTypes: make(map[string]bool)}, nil
		}
		return nil, err
	}
	var state State
	err = json.Unmarshal(data, &state)
	if state.LastPlayed == nil {
		state.LastPlayed = make(map[string]time.Time) // Ініціалізуємо порожню карту
	}
	return &state, err
}

func saveState(state *State, path string) {
	// Перетворюємо порожню карту LastPlayed у null для коректного збереження
	if len(state.LastPlayed) == 0 {
		state.LastPlayed = nil
	}

	data, err := json.Marshal(state)
	if err != nil {
		log.Printf("Помилка збереження стану: %v", err)
		return
	}
	err = os.WriteFile(path, data, 0644) // Заміщено ioutil.WriteFile на os.WriteFile
	if err != nil {
		log.Printf("Помилка запису стану у файл: %v", err)
	}

	// Відновлюємо порожню карту після збереження
	if state.LastPlayed == nil {
		state.LastPlayed = make(map[string]time.Time)
	}
}

func setupLogging(config *Config) {
	if config.LogToFile {
		logFile, err := os.OpenFile(config.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("Помилка відкриття файлу логу: %v", err)
		}
		multiWriter := io.MultiWriter(os.Stdout, logFile)
		log.SetOutput(multiWriter)
	} else {
		log.SetOutput(os.Stdout)
	}
}

func convertToLocalTime(utcTime string, timeZone string) string {
	// Перетворюємо час UTC у локальну часову зону
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		log.Printf("Помилка завантаження часової зони: %v", err)
		return utcTime // Повертаємо UTC, якщо часова зона недоступна
	}
	parsedTime, err := time.Parse(time.RFC3339, utcTime)
	if err != nil {
		log.Printf("Помилка парсингу часу у форматі RFC3339: %v", err)
		return utcTime
	}
	return parsedTime.In(location).Format("2006-01-02 15:04:05")
}

func checkAndHandleStateChange(state *State, currentAlerts map[string]bool, alerts []Alert, lastUpdate string, config *Config, statePath string) {
	// Перевіряємо нові події
	var selectedAlert *Alert
	for _, alert := range alerts {
		if alert.Type == "AIR" {
			selectedAlert = &alert
			break
		}
	}

	// Якщо події з type: AIR немає, вибираємо найраніше за lastUpdate
	if selectedAlert == nil && len(alerts) > 0 {
		selectedAlert = &alerts[0]
		for _, alert := range alerts {
			if alert.LastUpdate < selectedAlert.LastUpdate {
				selectedAlert = &alert
			}
		}
	}

	// Якщо вибрано подію, обробляємо її
	if selectedAlert != nil {
		alertType := selectedAlert.Type
		if !state.ActiveAlertTypes[alertType] {
			// Нова подія — зберігаємо стан і відтворюємо звук початку події
			state.ActiveAlertTypes[alertType] = true
			state.LastPlayed[alertType] = time.Now().UTC() // Встановлюємо поточний час для події
			saveState(state, statePath)
			log.Printf("Подія увімкнено: %s, час: %s", alertType, selectedAlert.LastUpdate)
			playAudio(config.AudioFiles[alertType])
		}
	}

	// Логуємо стан активних подій
	for alertType := range state.ActiveAlertTypes {
		localTime := convertToLocalTime(lastUpdate, config.TimeZone)
		log.Printf("Триває тривога від %s для події: %s", localTime, alertType)
	}

	// Перевіряємо зниклі події
	for alertType := range state.ActiveAlertTypes {
		if !currentAlerts[alertType] {
			// Подія зникла — зберігаємо стан і відтворюємо звук закінчення події
			delete(state.ActiveAlertTypes, alertType)
			saveState(state, statePath)
			log.Printf("Подія вимкнено: %s, час завершення: %s", alertType, lastUpdate)
			playAudio(config.AlertOnEmpty)
		}
	}
}

func checkAndPlayRepeatAudio(state *State, config *Config, location *time.Location, statePath string) {
	if !config.EnableRepeatAudio || config.RepeatAudioFile == "" || config.RepeatIntervalMin <= 0 {
		return // Виходимо, якщо повторюваний сигнал вимкнено або параметри некоректні
	}

	// Вибираємо подію для відтворення повторного звуку
	var selectedAlertType string
	for alertType := range state.ActiveAlertTypes {
		if selectedAlertType == "" || alertType == "AIR" {
			selectedAlertType = alertType
		}
	}

	// Перевіряємо, чи потрібно відтворити повторний звук для вибраної події
	if selectedAlertType != "" {
		lastUpdateTime, err := time.Parse(time.RFC3339, state.LastUpdate)
		if err != nil {
			log.Printf("Помилка парсингу часу last_update: %v", err)
			return
		}

		now := time.Now().UTC()
		elapsedMinutes := int(now.Sub(lastUpdateTime).Minutes())

		// Розраховуємо, чи має відтворюватися повторна подія
		if elapsedMinutes >= config.RepeatIntervalMin && elapsedMinutes%config.RepeatIntervalMin == 0 {
			log.Printf("Відтворення повторного звуку для події: %s", selectedAlertType)
			playAudio(config.RepeatAudioFile)
		}
	}
}

func printConfigDescription() {
	fmt.Println("Опис файлу конфігурації:")
	fmt.Println(`
{
  "api_url": "URL для API запитів",
  "auth_header": "Заголовок авторизації для API",
  "audio_files": {
    "AIR": "Шлях до аудіофайлу для події AIR",
    "FIRE": "Шлях до аудіофайлу для події FIRE"
  },
  "alert_on_empty": "Шлях до аудіофайлу для події, коли масив порожній",
  "debug": true, // Увімкнення режиму налагодження
  "log_to_file": true, // Увімкнення дублювання логу у файл
  "log_file_path": "Шлях до файлу логу",
  "time_zone": "Локальна часова зона, наприклад, Europe/Kiev",
  "repeat_audio_file": "Шлях до аудіофайлу для повторного відтворення",
  "repeat_interval_min": 10 // Інтервал повторного відтворення у хвилинах
  "request_interval_sec": 30 // Інтервал запитів до сервера у секундах
}
    `)
}
