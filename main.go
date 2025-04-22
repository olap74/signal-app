package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
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
	// Парсим флаги
	configPath := flag.String("config", "config.json", "Путь к файлу настроек")
	statePath := flag.String("state", "state.json", "Путь к файлу состояния")
	help := flag.Bool("help", false, "Вывести информацию о настройках и выйти")
	configDesc := flag.Bool("config-desc", false, "Вывести описание файла конфигурации и выйти")
	flag.Parse()

	// Если указан флаг help, выводим информацию о настройках
	if *help {
		fmt.Println("Программа для мониторинга событий и воспроизведения аудио.")
		fmt.Println("Доступные флаги:")
		flag.PrintDefaults()
		return
	}

	// Если указан флаг config-desc, выводим описание файла конфигурации
	if *configDesc {
		printConfigDescription()
		return
	}

	// Загружаем конфигурацию
	config, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Ошибка загрузки конфигурации: %v", err)
	}

	// Настраиваем логирование
	setupLogging(config)

	// Загружаем предыдущее состояние
	state, err := loadState(*statePath)
	if err != nil {
		log.Printf("Не удалось загрузить предыдущее состояние: %v", err)
		state = &State{
			ActiveAlertTypes: make(map[string]bool),
			LastUpdate:       "неизвестно",
			LastPlayed:       make(map[string]time.Time),
		}
	}

	// Определяем локальную временную зону
	location, err := time.LoadLocation(config.TimeZone)
	if err != nil {
		log.Fatalf("Ошибка загрузки временной зоны: %v", err)
	}

	// Делаем первый запрос к API
	alerts, lastUpdate, err := fetchAlerts(config)
	if err != nil {
		log.Fatalf("Ошибка получения данных при запуске: %v", err)
	}

	// Преобразуем время lastUpdate в локальную временную зону
	lastUpdateLocal := convertToLocalTime(lastUpdate, location, len(alerts) > 0)

	// Создаем карту текущих активных типов событий
	currentAlerts := make(map[string]bool)
	for _, alert := range alerts {
		currentAlerts[alert.Type] = true
	}

	// Проверяем изменения состояния при запуске
	checkAndHandleStateChange(state, currentAlerts, lastUpdateLocal, config, *statePath)

	// Выводим текущее состояние при запуске
	printInitialState(state)

	// Устанавливаем интервал запросов к серверу
	requestInterval := time.Duration(config.RequestIntervalSec) * time.Second
	if config.RequestIntervalSec <= 0 {
		requestInterval = 30 * time.Second // Значение по умолчанию
	}

	// Основной цикл
	for {
		// Делаем запрос к API
		alerts, lastUpdate, err := fetchAlerts(config)
		if err != nil {
			log.Printf("Ошибка получения данных: %v", err)
			time.Sleep(requestInterval)
			continue
		}

		// Проверяем, есть ли активные тревоги
		hasActiveAlerts := len(alerts) > 0

		// Преобразуем время lastUpdate в локальную временную зону
		lastUpdateLocal := convertToLocalTime(lastUpdate, location, hasActiveAlerts)

		// Создаем карту текущих активных типов событий
		currentAlerts := make(map[string]bool)
		for _, alert := range alerts {
			currentAlerts[alert.Type] = true
		}

		// Проверяем изменения состояния
		checkAndHandleStateChange(state, currentAlerts, lastUpdateLocal, config, *statePath)

		// Проверяем необходимость воспроизведения повторного аудио
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
		log.Println("Аудиофайл не указан")
		return
	}

	f, err := os.Open(path)
	if err != nil {
		log.Printf("Ошибка открытия аудиофайла: %v", err)
		return
	}
	defer f.Close()

	streamer, format, err := mp3.Decode(f)
	if err != nil {
		log.Printf("Ошибка декодирования аудиофайла: %v", err)
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
		log.Printf("Ошибка сохранения состояния: %v", err)
		return
	}
	err = ioutil.WriteFile(path, data, 0644)
	if err != nil {
		log.Printf("Ошибка записи состояния в файл: %v", err)
	}
}

func setupLogging(config *Config) {
	if config.LogToFile {
		logFile, err := os.OpenFile(config.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("Ошибка открытия файла лога: %v", err)
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
		log.Printf("Ошибка парсинга времени в формате RFC3339: %v", err)
		return utcTime
	}
	localTime := parsedTime.In(location)
	if hasActiveAlerts {
		log.Printf("Тревога активна. Время начала тревоги: %s", localTime.Format("2006-01-02 15:04:05"))
	} else {
		log.Printf("Время окончания последней тревоги: %s", localTime.Format("2006-01-02 15:04:05"))
	}
	return localTime.Format("2006-01-02 15:04:05")
}

func printInitialState(state *State) {
	status := "выключено"
	if len(state.ActiveAlertTypes) > 0 {
		status = "включено"
	}
	log.Printf("Текущее состояние: %s, время последнего обновления: %s", status, state.LastUpdate)
}

func checkAndHandleStateChange(state *State, currentAlerts map[string]bool, lastUpdate string, config *Config, statePath string) {
	// Проверяем корректность времени lastUpdate
	parsedTime, err := time.Parse("2006-01-02 15:04:05", lastUpdate)
	if err != nil {
		log.Printf("Некорректное время начала события: %s. Устанавливаем текущее время.", lastUpdate)
		parsedTime = time.Now()
	} else {
		// Преобразуем время в локальную временную зону
		location, _ := time.LoadLocation(config.TimeZone)
		parsedTime = parsedTime.In(location)
	}
	lastUpdate = parsedTime.Format("2006-01-02 15:04:05")

	// Если время в state отличается от времени из ответа, обновляем его
	if state.LastUpdate != lastUpdate {
		log.Printf("Обновление времени в state.json: %s -> %s", state.LastUpdate, lastUpdate)
		state.LastUpdate = lastUpdate
		saveState(state, statePath)
	}

	// Обрабатываем новые события
	for alertType := range currentAlerts {
		if !state.ActiveAlertTypes[alertType] {
			// Новое событие — сохраняем состояние и проигрываем аудиофайл
			state.ActiveAlertTypes[alertType] = true
			saveState(state, statePath)
			log.Printf("Событие включено: %s, время: %s", alertType, lastUpdate)
			playAudio(config.AudioFiles[alertType])
		}
	}

	// Обрабатываем исчезнувшие события
	for alertType := range state.ActiveAlertTypes {
		if !currentAlerts[alertType] {
			// Событие исчезло — сохраняем состояние и проигрываем аудиофайл для пропадания
			delete(state.ActiveAlertTypes, alertType)
			saveState(state, statePath)
			log.Printf("Событие выключено: %s, время окончания: %s", alertType, lastUpdate)
			playAudio(config.AlertOnEmpty)
		}
	}
}

func checkAndPlayRepeatAudio(state *State, config *Config, location *time.Location, statePath string) {
	if config.RepeatAudioFile == "" || config.RepeatIntervalMin <= 0 {
		return
	}

	for alertType := range state.ActiveAlertTypes {
		lastUpdateTime, err := time.Parse("2006-01-02 15:04:05", state.LastUpdate)
		if err != nil {
			log.Printf("Ошибка парсинга времени lastUpdate: %v", err)
			continue
		}

		// Преобразуем lastUpdateTime в локальную временную зону
		lastUpdateTime = lastUpdateTime.In(location)

		// Рассчитываем текущее время и ближайший временной промежуток
		now := time.Now().In(location)
		elapsedMinutes := int(now.Sub(lastUpdateTime).Minutes())
		if elapsedMinutes < 0 {
			continue // Пропускаем, если текущее время меньше времени начала события
		}

		// Проверяем, пересекается ли текущее время с временными промежутками
		if elapsedMinutes%config.RepeatIntervalMin == 0 {
			if lastPlayed, exists := state.LastPlayed[alertType]; exists && lastPlayed.Equal(now.Truncate(time.Minute)) {
				continue // Пропускаем, если сигнал уже был воспроизведен в этом временном промежутке
			}

			log.Printf("Воспроизведение повторного аудио для события: %s в %s", alertType, now.Format("2006-01-02 15:04:05"))
			playAudio(config.RepeatAudioFile)
			state.LastPlayed[alertType] = now.Truncate(time.Minute)
			saveState(state, statePath)
		}
	}
}

func printConfigDescription() {
	fmt.Println("Описание файла конфигурации:")
	fmt.Println(`
{
  "api_url": "URL для API запросов",
  "auth_header": "Заголовок авторизации для API",
  "audio_files": {
    "AIR": "Путь к аудиофайлу для события AIR",
    "FIRE": "Путь к аудиофайлу для события FIRE"
  },
  "alert_on_empty": "Путь к аудиофайлу для события, когда массив пуст",
  "debug": true, // Включение режима отладки
  "log_to_file": true, // Включение дублирования лога в файл
  "log_file_path": "Путь к файлу лога",
  "time_zone": "Локальная временная зона, например, Europe/Kiev",
  "repeat_audio_file": "Путь к аудиофайлу для повторного воспроизведения",
  "repeat_interval_min": 10 // Интервал повторного воспроизведения в минутах
  "request_interval_sec": 30 // Интервал запросов к серверу в секундах
}
    `)
}
