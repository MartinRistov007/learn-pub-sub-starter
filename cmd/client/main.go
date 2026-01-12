package main

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/bootdotdev/learn-pub-sub-starter/internal/gamelogic"
	"github.com/bootdotdev/learn-pub-sub-starter/internal/pubsub"
	"github.com/bootdotdev/learn-pub-sub-starter/internal/routing"
	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	fmt.Println("Starting Peril client...")

	connString := "amqp://guest:guest@localhost:5672/"
	conn, err := amqp.Dial(connString)
	if err != nil {
		log.Fatalf("Failed to connect to RabbitMQ: %v", err)
	}
	defer conn.Close()
	fmt.Println("Peril client connected to RabbitMQ!")

	publishCh, err := conn.Channel()
	if err != nil {
		log.Fatalf("Failed to create publish channel: %v", err)
	}
	defer publishCh.Close()

	username, err := gamelogic.ClientWelcome()
	if err != nil {
		log.Fatalf("Could not get username: %v", err)
	}

	queueName := fmt.Sprintf("%s.%s", routing.PauseKey, username)

	_, _, err = pubsub.DeclareAndBind(
		conn,
		routing.ExchangePerilDirect,
		queueName,
		routing.PauseKey,
		pubsub.SimpleQueueTransient,
	)
	if err != nil {
		log.Fatalf("Could not subscribe to pause: %v", err)
	}
	fmt.Printf("Queue %v declared and bound!\n", queueName)

	gs := gamelogic.NewGameState(username)

	err = pubsub.SubscribeJSON(
		conn,
		routing.ExchangePerilDirect,
		fmt.Sprintf("%s.%s", routing.PauseKey, username),
		routing.PauseKey,
		pubsub.SimpleQueueTransient,
		handlerPause(gs),
	)
	if err != nil {
		log.Fatalf("Could not subscribe to pause: %v", err)
	}

	err = pubsub.SubscribeJSON(
		conn,
		routing.ExchangePerilTopic,
		fmt.Sprintf("%s.%s", routing.ArmyMovesPrefix, username),
		routing.ArmyMovesPrefix+".*",
		pubsub.SimpleQueueTransient,
		handlerMove(gs, publishCh),
	)
	if err != nil {
		log.Fatalf("Could not subscribe to move: %v", err)
	}

	err = pubsub.SubscribeJSON(
		conn,
		routing.ExchangePerilTopic,
		"war",
		routing.WarRecognitionsPrefix+".*",
		pubsub.SimpleQueueDurable,
		handlerWar(gs, publishCh),
	)
	if err != nil {
		log.Fatalf("Could not subscribe to war: %v", err)
	}

	for {
		words := gamelogic.GetInput()
		if len(words) == 0 {
			continue
		}

		cmd := words[0]

		switch cmd {
		case "spawn":
			err := gs.CommandSpawn(words)
			if err != nil {
				fmt.Println(err)
			}
		case "move":
			move, err := gs.CommandMove(words)
			if err != nil {
				fmt.Println(err)
				continue
			}

			err = pubsub.PublishJSON(
				publishCh,
				routing.ExchangePerilTopic,
				fmt.Sprintf("%s.%s", routing.ArmyMovesPrefix, username),
				move,
			)
			if err != nil {
				fmt.Printf("Error publishing move: %v\n", err)
				continue
			}
			fmt.Printf("Moved %v units to %s\n", len(move.Units), move.ToLocation)
		case "status":
			gs.CommandStatus()
		case "help":
			gamelogic.PrintClientHelp()
		case "spam":
			if len(words) < 2 {
				fmt.Println("Please provide an amount to spam.")
				continue
			}
			 n, err := strconv.Atoi(words[1])
			 if err != nil {
				fmt.Printf("Invalid amount: %v\n", err)
				continue
			 }

			 for i := 0; i < n; i++ {
				msg := gamelogic.GetMaliciousLog()
				err := pubsub.PublishGob(
					publishCh,
					routing.ExchangePerilTopic,
					fmt.Sprintf("%s.%s", routing.GameLogSlug, username),
					routing.GameLog{
						CurrentTime: time.Now(),
						Message: msg,
						Username: username,
					},
				)
				if err != nil {
					fmt.Printf("Error publishing malicious log: %v\n", err)
				}
			 }
			 fmt.Printf("Spammed %d logs\n", n)
		case "quit":
			gamelogic.PrintQuit()
			return
		default:
			fmt.Println("I don't understand that command.")
		}
	}
}

func handlerPause(gs *gamelogic.GameState) func(routing.PlayingState) pubsub.AckType{
	return func(ps routing.PlayingState) pubsub.AckType{
		defer fmt.Print("> ")
		gs.HandlePause(ps)
		return pubsub.Ack
	}
}

func handlerMove(gs *gamelogic.GameState, publishCh *amqp.Channel) func(gamelogic.ArmyMove) pubsub.AckType{
	return func(move gamelogic.ArmyMove) pubsub.AckType{
		defer fmt.Print("> ")
		outcome := gs.HandleMove(move)
		switch outcome {
		case gamelogic.MoveOutcomeSamePlayer:
			return pubsub.NackDiscard
		case gamelogic.MoveOutcomeMakeWar:
			err := pubsub.PublishJSON(
				publishCh,
				routing.ExchangePerilTopic,
				fmt.Sprintf("%s.%s", routing.WarRecognitionsPrefix, gs.GetUsername()),
				gamelogic.RecognitionOfWar{
					Attacker: move.Player,
					Defender: gs.GetPlayerSnap(),
				},
			)
			if err != nil {
				fmt.Printf("Error publishing war declaration: %v\n", err)
				return pubsub.NackRequeue
			}
			return pubsub.Ack
		case gamelogic.MoveOutComeSafe:
			return pubsub.Ack
		default:
			return pubsub.NackDiscard
		}
	}
}

func handlerWar(gs *gamelogic.GameState, publishCh *amqp.Channel) func(gamelogic.RecognitionOfWar) pubsub.AckType {
	return func(rw gamelogic.RecognitionOfWar) pubsub.AckType {
		defer fmt.Print("> ")
		outcome, winner, loser := gs.HandleWar(rw)

		switch outcome {
		case gamelogic.WarOutcomeNotInvolved:
			return pubsub.NackRequeue
		case gamelogic.WarOutcomeNoUnits:
			return pubsub.NackDiscard
		case gamelogic.WarOutcomeOpponentWon, gamelogic.WarOutcomeYouWon, gamelogic.WarOutcomeDraw:
			var msg string
			if outcome == gamelogic.WarOutcomeDraw {
				msg = fmt.Sprintf("A war between %s and %s resulted in a draw", winner, loser)
			} else {
				msg = fmt.Sprintf("%s won a war against %s", winner, loser)
			}

			gamelog := routing.GameLog{
				CurrentTime: time.Now(),
				Message: msg, 
				Username: gs.GetUsername(),
			}

			err := pubsub.PublishGob(
				publishCh,
				routing.ExchangePerilTopic,
				fmt.Sprintf("%s.%s", routing.GameLogSlug, gs.GetUsername()),
				gamelog,
			)

			if err != nil {
				fmt.Printf("Error publishing log: %v\n", err)
				return pubsub.NackRequeue
			}
			return pubsub.Ack
		default:
			fmt.Println("Error: Unknown war outcome")
			return pubsub.NackDiscard
		}
	}
}
