package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultWeatherTimeout    = 12 * time.Second
	openMeteoGeoURL          = "https://geocoding-api.open-meteo.com/v1/search"
	openMeteoForecastBaseURL = "https://api.open-meteo.com/v1/forecast"
)

type WeatherTool struct {
	client *http.Client
}

type weatherParams struct {
	Location string `json:"location"`
}

type openMeteoGeoResponse struct {
	Results []openMeteoGeoResult `json:"results"`
}

type openMeteoGeoResult struct {
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Country   string  `json:"country"`
	Timezone  string  `json:"timezone"`
}

type openMeteoForecastResponse struct {
	Timezone     string `json:"timezone"`
	CurrentUnits struct {
		Temperature2M string `json:"temperature_2m"`
		WeatherCode   string `json:"weather_code"`
		WindSpeed10M  string `json:"wind_speed_10m"`
	} `json:"current_units"`
	Current struct {
		Time          string  `json:"time"`
		Temperature2M float64 `json:"temperature_2m"`
		WeatherCode   int     `json:"weather_code"`
		WindSpeed10M  float64 `json:"wind_speed_10m"`
	} `json:"current"`
	DailyUnits struct {
		Time             string `json:"time"`
		WeatherCode      string `json:"weather_code"`
		Temperature2MMax string `json:"temperature_2m_max"`
		Temperature2MMin string `json:"temperature_2m_min"`
	} `json:"daily_units"`
	Daily struct {
		Time             []string  `json:"time"`
		WeatherCode      []int     `json:"weather_code"`
		Temperature2MMax []float64 `json:"temperature_2m_max"`
		Temperature2MMin []float64 `json:"temperature_2m_min"`
	} `json:"daily"`
}

func NewWeatherTool() Tool {
	return &WeatherTool{
		client: &http.Client{Timeout: defaultWeatherTimeout},
	}
}

func (t *WeatherTool) GetName() string {
	return "weather"
}

func (t *WeatherTool) GetDescription() string {
	return "Gets weather for a location, including current conditions and a 7-day forecast."
}

func (t *WeatherTool) GetFunction() func(params string) string {
	return func(params string) string {
		var in weatherParams
		if err := json.Unmarshal([]byte(params), &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}

		location := strings.TrimSpace(in.Location)
		if location == "" {
			return errorJSON("location is required")
		}

		geo, err := t.geocode(location)
		if err != nil {
			return errorJSON(err.Error())
		}

		forecast, err := t.forecast(geo)
		if err != nil {
			return errorJSON(err.Error())
		}

		days := make([]map[string]any, 0, len(forecast.Daily.Time))
		for i := range forecast.Daily.Time {
			maxTemp := 0.0
			minTemp := 0.0
			code := 0
			if i < len(forecast.Daily.Temperature2MMax) {
				maxTemp = forecast.Daily.Temperature2MMax[i]
			}
			if i < len(forecast.Daily.Temperature2MMin) {
				minTemp = forecast.Daily.Temperature2MMin[i]
			}
			if i < len(forecast.Daily.WeatherCode) {
				code = forecast.Daily.WeatherCode[i]
			}

			days = append(days, map[string]any{
				"date":         forecast.Daily.Time[i],
				"min_temp":     minTemp,
				"max_temp":     maxTemp,
				"weather_code": code,
				"description":  weatherCodeDescription(code),
			})
		}

		payload := map[string]any{
			"location": map[string]any{
				"query":     location,
				"name":      geo.Name,
				"country":   geo.Country,
				"timezone":  forecast.Timezone,
				"latitude":  geo.Latitude,
				"longitude": geo.Longitude,
			},
			"current": map[string]any{
				"time":         forecast.Current.Time,
				"temperature":  forecast.Current.Temperature2M,
				"weather_code": forecast.Current.WeatherCode,
				"description":  weatherCodeDescription(forecast.Current.WeatherCode),
				"wind_speed":   forecast.Current.WindSpeed10M,
				"units": map[string]string{
					"temperature": forecast.CurrentUnits.Temperature2M,
					"wind_speed":  forecast.CurrentUnits.WindSpeed10M,
				},
			},
			"daily": map[string]any{
				"days": days,
				"units": map[string]string{
					"min_temp": forecast.DailyUnits.Temperature2MMin,
					"max_temp": forecast.DailyUnits.Temperature2MMax,
				},
			},
		}

		out, err := json.Marshal(payload)
		if err != nil {
			return errorJSON("serialize weather result failed: " + err.Error())
		}
		return string(out)
	}
}

func (t *WeatherTool) IsImmediate() bool {
	return false
}

func (t *WeatherTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"location": map[string]any{
				"type":        "string",
				"description": "Location string, e.g. 'Rome, Italy' or 'New York, US'.",
			},
		},
		Required: []string{"location"},
	}
}

func (t *WeatherTool) geocode(location string) (openMeteoGeoResult, error) {
	u, err := url.Parse(openMeteoGeoURL)
	if err != nil {
		return openMeteoGeoResult{}, fmt.Errorf("invalid geocoding url: %w", err)
	}
	q := u.Query()
	q.Set("name", location)
	q.Set("count", "1")
	q.Set("language", "en")
	q.Set("format", "json")
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return openMeteoGeoResult{}, fmt.Errorf("build geocoding request failed: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return openMeteoGeoResult{}, fmt.Errorf("geocoding request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return openMeteoGeoResult{}, fmt.Errorf("geocoding returned status %d", resp.StatusCode)
	}

	var out openMeteoGeoResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return openMeteoGeoResult{}, fmt.Errorf("decode geocoding response failed: %w", err)
	}
	if len(out.Results) == 0 {
		return openMeteoGeoResult{}, fmt.Errorf("location not found: %s", location)
	}

	return out.Results[0], nil
}

func (t *WeatherTool) forecast(geo openMeteoGeoResult) (openMeteoForecastResponse, error) {
	u, err := url.Parse(openMeteoForecastBaseURL)
	if err != nil {
		return openMeteoForecastResponse{}, fmt.Errorf("invalid forecast url: %w", err)
	}
	q := u.Query()
	q.Set("latitude", fmt.Sprintf("%f", geo.Latitude))
	q.Set("longitude", fmt.Sprintf("%f", geo.Longitude))
	q.Set("current", "temperature_2m,weather_code,wind_speed_10m")
	q.Set("daily", "weather_code,temperature_2m_max,temperature_2m_min")
	q.Set("forecast_days", "7")
	if strings.TrimSpace(geo.Timezone) != "" {
		q.Set("timezone", geo.Timezone)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return openMeteoForecastResponse{}, fmt.Errorf("build forecast request failed: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return openMeteoForecastResponse{}, fmt.Errorf("forecast request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return openMeteoForecastResponse{}, fmt.Errorf("forecast returned status %d", resp.StatusCode)
	}

	var out openMeteoForecastResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return openMeteoForecastResponse{}, fmt.Errorf("decode forecast response failed: %w", err)
	}
	return out, nil
}

func weatherCodeDescription(code int) string {
	switch code {
	case 0:
		return "clear sky"
	case 1, 2, 3:
		return "partly cloudy"
	case 45, 48:
		return "fog"
	case 51, 53, 55:
		return "drizzle"
	case 56, 57:
		return "freezing drizzle"
	case 61, 63, 65:
		return "rain"
	case 66, 67:
		return "freezing rain"
	case 71, 73, 75, 77:
		return "snow"
	case 80, 81, 82:
		return "rain showers"
	case 85, 86:
		return "snow showers"
	case 95:
		return "thunderstorm"
	case 96, 99:
		return "thunderstorm with hail"
	default:
		return "unknown"
	}
}
