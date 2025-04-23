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
	EnableRepeatAudio  bool              `json:"enable_repeat_audio"`
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
			LastUpdate:       "невідомо",
			LastPlayed:       make(map[string]time.Time),
		}
	}

	// Визначаємо локальну часову зону
	location, err := time.LoadLocation(config.TimeZone)
	if err != nil {
		log.Fatalf("Помилка завантаження часової зони: %v", err)
	}

	// Робимо перший запит до API
	alerts, lastUpdate, err := fetchAlerts(config)
	if err != nil {
		log.Fatalf("Помилка отримання даних під час запуску: %v", err)
	}

	// Перетворюємо час lastUpdate у локальну часову зону
	lastUpdateLocal := convertToLocalTime(lastUpdate, location, len(alerts) > 0)

	// Створюємо карту поточних активних типів подій
	currentAlerts := make(map[string]bool)
	for _, alert := range alerts {
		currentAlerts[alert.Type] = true
	}

	// Перевіряємо зміни стану під час запуску
	checkAndHandleStateChange(state, currentAlerts, alerts, lastUpdateLocal, config, *statePath)

	// Виводимо поточний стан під час запуску
	printInitialState(state)

	// Встановлюємо інтервал запитів до сервера
	requestInterval := time.Duration(config.RequestIntervalSec) * time.Second
	if config.RequestIntervalSec <= 0 {
		requestInterval = 30 * time.Second // Значення за замовчуванням
	}

	// Основний цикл
	for {
		// Робимо запит до API
		alerts, lastUpdate, err := fetchAlerts(config)
		if err != nil {
			log.Printf("Помилка отримання даних: %v", err)
			time.Sleep(requestInterval)
			continue
		}

		// Перевіряємо, чи є активні тривоги
		hasActiveAlerts := len(alerts) > 0

		// Перетворюємо час lastUpdate у локальну часову зону
		lastUpdateLocal := convertToLocalTime(lastUpdate, location, hasActiveAlerts)

		// Створюємо карту поточних активних типів подій
		currentAlerts := make(map[string]bool)
		for _, alert := range alerts {
			currentAlerts[alert.Type] = true
		}

		// Перевіряємо зміни стану
		checkAndHandleStateChange(state, currentAlerts, alerts, lastUpdateLocal, config, *statePath)

		// Перевіряємо необхідність відтворення повторного аудіо
		checkAndPlayRepeatAudio(state, config, location, *statePath)

		time.Sleep(requestInterval)
	}
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) // Заменено ioutil.ReadFile на os.ReadFile
	if err != nil {
		return nil, err
	}

	// Удаляем комментарии из JSON
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

	// Устанавливаем заголовок авторизации
	req.Header.Set("Authorization", config.AuthHeader)

	if config.Debug {
		log.Printf("Отправка запроса: %s", config.APIURL)
		// log.Printf("Заголовок Authorization: %s", config.AuthHeader) // Убрано из логов
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if config.Debug {
		log.Printf("Получен ответ: %d", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("неожиданный статус ответа: %d", resp.StatusCode)
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
	data, err := os.ReadFile(path) // Заменено ioutil.ReadFile на os.ReadFile
	if err != nil {
		if os.IsNotExist(err) {
			return &State{ActiveAlertTypes: make(map[string]bool)}, nil
		}
		return nil, err
	}
	var state State
	err = json.Unmarshal(data, &state)
	if state.LastPlayed == nil {
		state.LastPlayed = make(map[string]time.Time)
	}
	return &state, err
}

func saveState(state *State, path string) {
	data, err := json.Marshal(state)
	if err != nil {
		log.Printf("Помилка збереження стану: %v", err)
		return
	}
	err = os.WriteFile(path, data, 0644) // Заменено ioutil.WriteFile на os.WriteFile
	if err != nil {
		log.Printf("Помилка запису стану у файл: %v", err)
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

func convertToLocalTime(utcTime string, location *time.Location, hasActiveAlerts bool) string {
	parsedTime, err := time.Parse(time.RFC3339, utcTime)
	if err != nil {
		log.Printf("Помилка парсингу часу у форматі RFC3339: %v", err)
		return utcTime
	}
	localTime := parsedTime.In(location)
	if hasActiveAlerts {
		log.Printf("Тривога активна. Час початку тривоги: %s", localTime.Format("2006-01-02 15:04:05"))
	} else {
		log.Printf("Час завершення останньої тривоги: %s", localTime.Format("2006-01-02 15:04:05"))
	}
	return localTime.Format("2006-01-02 15:04:05")
}

func printInitialState(state *State) {
	status := "вимкнено"
	if len(state.ActiveAlertTypes) > 0 {
		status = "увімкнено"
	}
	log.Printf("Поточний стан: %s, час останнього оновлення: %s", status, state.LastUpdate)
}

func checkAndHandleStateChange(state *State, currentAlerts map[string]bool, alerts []Alert, lastUpdate string, config *Config, statePath string) {
	var relevantLastUpdate string

	// Якщо подія активна, беремо lastUpdate з першого елемента activeAlerts
	if len(alerts) > 0 {
		relevantLastUpdate = alerts[0].LastUpdate
	} else {
		// Якщо подія неактивна, беремо lastUpdate з відповіді сервера
		relevantLastUpdate = lastUpdate
	}

	// Парсимо час relevantLastUpdate як RFC3339
	parsedTime, err := time.Parse(time.RFC3339, relevantLastUpdate)
	if err != nil {
		log.Printf("Некоректний час початку події: %s. Встановлюємо поточний час.", relevantLastUpdate)
		parsedTime = time.Now().UTC() // Встановлюємо поточний час в UTC
	}
	relevantLastUpdateUTC := parsedTime.Format(time.RFC3339) // Зберігаємо у форматі UTC

	// Якщо час у state відрізняється від часу з відповіді, оновлюємо його
	if state.LastUpdate != relevantLastUpdateUTC {
		log.Printf("Оновлення часу у state.json: %s -> %s", state.LastUpdate, relevantLastUpdateUTC)
		state.LastUpdate = relevantLastUpdateUTC
		saveState(state, statePath)
	}

	// Обробляємо нові події
	for alertType := range currentAlerts {
		if !state.ActiveAlertTypes[alertType] {
			// Нова подія — зберігаємо стан та відтворюємо звук початку події
			state.ActiveAlertTypes[alertType] = true
			saveState(state, statePath)
			log.Printf("Подія увімкнено: %s, час: %s", alertType, relevantLastUpdateUTC)
			playAudio(config.AudioFiles[alertType])
		}
	}

	// Обробляємо зниклі події
	for alertType := range state.ActiveAlertTypes {
		if !currentAlerts[alertType] {
			// Подія зникла — зберігаємо стан та відтворюємо звук завершення події
			delete(state.ActiveAlertTypes, alertType)
			saveState(state, statePath)
			log.Printf("Подія вимкнено: %s, час завершення: %s", alertType, relevantLastUpdateUTC)
			playAudio(config.AlertOnEmpty)
		}
	}
}

func checkAndPlayRepeatAudio(state *State, config *Config, location *time.Location, statePath string) {
	if !config.EnableRepeatAudio || config.RepeatAudioFile == "" || config.RepeatIntervalMin <= 0 {
		return
	}

	for alertType := range state.ActiveAlertTypes {
		lastUpdateTime, err := time.Parse(time.RFC3339, state.LastUpdate)
		if err != nil {
			log.Printf("Помилка парсингу часу lastUpdate: %v", err)
			continue
		}

		// Перетворюємо lastUpdateTime у локальну часову зону
		lastUpdateTime = lastUpdateTime.In(location)

		// Розраховуємо поточний час
		now := time.Now().In(location)

		// Перевіряємо, чи перетинається поточний час з часовими проміжками від часу початку події
		elapsedMinutes := int(now.Sub(lastUpdateTime).Minutes())
		if elapsedMinutes < 0 {
			continue // Пропускаємо, якщо поточний час менший за час початку події
		}

		// Перевіряємо, чи потрібно відтворити сигнал
		if elapsedMinutes%config.RepeatIntervalMin == 0 {
			if lastPlayed, exists := state.LastPlayed[alertType]; exists && lastPlayed.Equal(now.Truncate(time.Minute)) {
				continue // Пропускаємо, якщо сигнал вже був відтворений у цьому часовому проміжку
			}

			log.Printf("Відтворення повторного аудіо для події: %s о %s", alertType, now.Format("2006-01-02 15:04:05"))
			playAudio(config.RepeatAudioFile)
			state.LastPlayed[alertType] = now.Truncate(time.Minute)
			saveState(state, statePath)
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
