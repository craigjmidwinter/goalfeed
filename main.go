package main

import (
	"fmt"
	mlbClients "goalfeed/clients/leagues/mlb"
	nhlClients "goalfeed/clients/leagues/nhl"
	"goalfeed/config"
	"goalfeed/models"
	"goalfeed/services/leagues"
	"goalfeed/services/leagues/mlb"
	"goalfeed/services/leagues/nhl"
	"goalfeed/targets/database"
	"goalfeed/targets/pusher"
	"goalfeed/targets/redis"
	"goalfeed/utils"
	"sync"
	"time"

	"github.com/bugsnag/bugsnag-go/v2"
	"github.com/joho/godotenv"
)

var (
	leagueServices = map[int]leagues.ILeagueService{}
	needRefresh    = false
	logger         = utils.GetLogger()
)

func init() {
	_ = godotenv.Load()
	bugsnag.Configure(bugsnag.Configuration{
		APIKey:          config.GetString("BUGSNAG_API_KEY"),
		ReleaseStage:    config.GetString("RELEASE_STAGE"),
		ProjectPackages: []string{"main", "github.com/org/myapp"},
	})
}

func main() {
	initialize()
	runTickers()
}

func runTickers() {
	var wg sync.WaitGroup
	tickers := []struct {
		duration time.Duration
		task     func()
	}{
		{1 * time.Minute, checkLeaguesForActiveGames},
		{1 * time.Second, watchActiveGames},
		{1 * time.Minute, sendTestGoal},
		{5 * time.Second, func() {
			if needRefresh {
				checkLeaguesForActiveGames()
				needRefresh = false
			}
		}},
	}

	for _, t := range tickers {
		wg.Add(1)
		go func(duration time.Duration, task func()) {
			defer wg.Done()
			ticker := time.NewTicker(duration)
			for range ticker.C {
				go task()
			}
		}(t.duration, t.task)
	}

	wg.Wait()
}

func initialize() {
	logger.Info("Init DB")
	database.InitializeDatabase()
	logger.Info("Puck Drop! Initializing Goalfeed Process")

	leagueServices[models.LeagueIdNHL] = nhl.NHLService{Client: nhlClients.NHLApiClient{}}
	leagueServices[models.LeagueIdMLB] = mlb.MLBService{Client: mlbClients.MLBApiClient{}}

	logger.Info("Initializing Active Games")
	checkLeaguesForActiveGames()
}

func checkLeaguesForActiveGames() {
	logger.Info("Updating Active Games")
	for _, service := range leagueServices {
		go checkForNewActiveGames(service)
	}
}

func checkForNewActiveGames(service leagues.ILeagueService) {
	logger.Info(fmt.Sprintf("Checking for active %s games", service.GetLeagueName()))
	gamesChan := make(chan []models.Game)
	go service.GetActiveGames(gamesChan)
	for _, game := range <-gamesChan {
		if !gameIsMonitored(game) {
			logger.Info(fmt.Sprintf("Adding %s game (%s @ %s) to active monitored games", service.GetLeagueName(), game.CurrentState.Away.Team.TeamCode, game.CurrentState.Home.Team.TeamCode))
			redis.SetGame(game)
			redis.AppendActiveGame(game)
		}
	}
}

func gameIsMonitored(game models.Game) bool {
	for _, activeGameKey := range redis.GetActiveGameKeys() {
		if activeGameKey == game.GetGameKey() {
			return true
		}
	}
	return false
}

func watchActiveGames() {
	for _, gameKey := range redis.GetActiveGameKeys() {
		go checkGame(gameKey)
	}
}

func checkGame(gameKey string) {
	game, err := redis.GetGameByGameKey(gameKey)
	if err != nil {
		logger.Error(err.Error())
		logger.Error(fmt.Sprintf("[%s] Game not found, skipping", gameKey))
		redis.DeleteActiveGameKey(gameKey)
		needRefresh = true
		return
	}

	service := leagueServices[int(game.LeagueId)]
	logger.Info(fmt.Sprintf("[%s - %s @ %s] Checking", service.GetLeagueName(), game.CurrentState.Away.Team.TeamCode, game.CurrentState.Home.Team.TeamCode))
	game.IsFetching = true
	redis.SetGame(game)

	updateChan := make(chan models.GameUpdate)
	eventChan := make(chan []models.Event)
	go service.GetGameUpdate(game, updateChan)
	update := <-updateChan
	go service.GetEvents(update, eventChan)
	go fireGoalEvents(eventChan, game)
	game.CurrentState = update.NewState

	if game.CurrentState.Status == models.StatusEnded {
		logger.Info(fmt.Sprintf("[%s - %s @ %s] Game has ended", service.GetLeagueName(), game.CurrentState.Away.Team.TeamCode, game.CurrentState.Home.Team.TeamCode))
		redis.DeleteActiveGame(game)
	} else {
		game.IsFetching = false
		redis.SetGame(game)
	}
}

func fireGoalEvents(events chan []models.Event, game models.Game) {
	for _, event := range <-events {
		logger.Info(fmt.Sprintf("Goal %s", event.TeamCode))
		go pusher.SendEvent(event)
		var scoringTeam models.Team
		if event.TeamCode == game.CurrentState.Home.Team.TeamCode {
			scoringTeam = game.CurrentState.Home.Team
		} else {
			scoringTeam = game.CurrentState.Away.Team
		}
		go database.InsertGoal(scoringTeam)
	}
}

func sendTestGoal() {
	logger.Info("Sending test goal")
	go pusher.SendEvent(models.Event{
		TeamCode:   "TEST",
		TeamName:   "TEST",
		LeagueId:   0,
		LeagueName: "TEST",
		TeamHash:   "TESTTEST",
	})
}
