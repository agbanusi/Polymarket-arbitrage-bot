package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"polymarket-bot/config"
	"polymarket-bot/internal/clients/clob"
	"polymarket-bot/internal/clients/gamma"
	"polymarket-bot/internal/clients/rtds"
	"polymarket-bot/internal/risk"
	"polymarket-bot/internal/services"
	"polymarket-bot/internal/strategies/crypto"
	"polymarket-bot/internal/strategies/overunder"
	"polymarket-bot/internal/strategies/sports"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Println("==============================================")
	log.Println("       POLYMARKET ARBITRAGE BOT               ")
	log.Println("==============================================")
	log.Println("Starting initialization...")

	// 1. Load Configuration
	cfg := config.LoadConfig()
	cfg.LogConfig()

	if cfg.IsDryRun() {
		log.Println("🔵 RUNNING IN DRY-RUN MODE - No real trades will be executed")
	} else {
		log.Println("🔴 RUNNING IN LIVE MODE - Real trades will be executed!")
		time.Sleep(3 * time.Second) // Give user time to cancel
	}

	// 2. Initialize API Clients
	log.Println("Initializing API clients...")
	gammaClient := gamma.NewClient(cfg)
	clobClient := clob.NewClient(cfg)
	rtdsClient := rtds.NewClient()

	// 3. Connect WebSocket (RTDS) for real-time prices
	go func() {
		if err := rtdsClient.Connect(); err != nil {
			log.Printf("RTDS: Connection failed: %v (continuing without real-time data)", err)
		} else {
			// Subscribe to crypto prices for the crypto strategy
			if cfg.CryptoEnabled {
				rtdsClient.SubscribeCryptoPrices("btcusdt", "ethusdt", "solusdt")
			}
		}
	}()

	// 4. Initialize Risk Manager
	log.Println("Initializing risk manager...")
	riskManager := risk.NewManager(cfg)

	// 5. Initialize Position Monitor
	log.Println("Initializing position monitor...")
	positionMonitor := services.NewPositionMonitor(cfg, clobClient, riskManager)

	// 6. Initialize Strategies
	log.Println("Initializing strategies...")
	sportsStrategy := sports.NewStrategy(cfg, gammaClient, clobClient, riskManager)
	ouStrategy := overunder.NewStrategy(cfg, gammaClient, clobClient, riskManager)
	cryptoStrategy := crypto.NewStrategy(cfg, gammaClient, clobClient, riskManager)

	// 7. Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())

	// 8. Start all components in goroutines
	log.Println("Starting bot components...")
	
	// Start position monitor
	go positionMonitor.Run()

	// Start strategies
	if cfg.SportsEnabled {
		go sportsStrategy.Run()
		log.Println("✅ Sports MONEYLINE strategy started")
		
		go ouStrategy.Run()
		log.Println("✅ Sports OVER/UNDER strategy started")
	} else {
		log.Println("⚪ Sports strategies disabled")
	}

	if cfg.CryptoEnabled {
		go cryptoStrategy.Run()
		log.Println("✅ Crypto strategy started")
	} else {
		log.Println("⚪ Crypto strategy disabled")
	}

	// 9. Start RTDS message processor (for real-time price updates)
	go processRTDSUpdates(ctx, rtdsClient, riskManager)

	// 10. Status logging
	go logPeriodicStatus(ctx, sportsStrategy, ouStrategy, cryptoStrategy, riskManager)

	log.Println("==============================================")
	log.Println("Bot is now running. Press Ctrl+C to stop.")
	log.Println("==============================================")

	// 11. Wait for shutdown signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("\n🛑 Shutdown signal received...")
	
	// Graceful shutdown
	cancel()
	
	// Stop strategies
	sportsStrategy.Stop()
	ouStrategy.Stop()
	cryptoStrategy.Stop()
	positionMonitor.Stop()
	
	// Close connections
	rtdsClient.Close()

	log.Println("Final position summary:")
	log.Println(riskManager.GetPositionSummary())
	log.Println("Bot shutdown complete.")
}

// processRTDSUpdates handles real-time price updates from WebSocket
func processRTDSUpdates(ctx context.Context, client *rtds.Client, rm *risk.Manager) {
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-client.CryptoPrices:
			log.Printf("RTDS: %s = $%.2f", update.Symbol, update.Price)
			// These are underlying crypto prices, not outcome token prices
			// Useful for correlating with market movements
		case update := <-client.MarketUpdates:
			// Update position prices directly from market feed
			rm.UpdatePrice(update.AssetID, update.Price)
		}
	}
}

// logPeriodicStatus logs strategy status and detailed position reports every 2 minutes (testing)
func logPeriodicStatus(ctx context.Context, sportsStrat *sports.Strategy, ouStrat *overunder.Strategy, cryptoStrat *crypto.Strategy, rm *risk.Manager) {
	// Print immediate report after 30 seconds
	time.Sleep(30 * time.Second)
	log.Println("=== INITIAL STATUS REPORT ===")
	log.Println(sportsStrat.GetStatus())
	log.Println(ouStrat.GetStatus())
	log.Println(cryptoStrat.GetStatus())
	log.Print(rm.GetDetailedReport())
	log.Println("=============================")

	reportTicker := time.NewTicker(2 * time.Minute)
	defer reportTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-reportTicker.C:
			log.Println("=== 2 MINUTE STATUS REPORT ===")
			log.Println(sportsStrat.GetStatus())
			log.Println(ouStrat.GetStatus())
			log.Println(cryptoStrat.GetStatus())
			log.Print(rm.GetDetailedReport())
			log.Println("==============================")
		}
	}
}

