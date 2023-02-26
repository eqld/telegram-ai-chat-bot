package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/mattn/go-sqlite3"
	gpt3 "github.com/sashabaranov/go-gpt3"
)

const (
	initTimeout                      = 60 * time.Second
	telegramBotUpdaterTimeoutSeconds = 60
	sqlDatabaseDriverName            = "sqlite3"

	defaultMaxMessagesInHistory         = 101
	defaultMaxTokensToGenerate          = 301
	defaultSQLMigrationsDirPathRelative = "database/migrations/"
	defaultApplicationDataRootDirPath   = "/data"
	defaultDatabaseFilename             = "db.sqlite"

	ps                     = string(os.PathSeparator)
	databaseDateTimeLayout = "2006-01-02 15:04:05.999999999Z07:00"

	gptModel                 = gpt3.GPT3TextDavinci003
	gptModelContextLengthMax = 4097
	gptContextInitial        = "The following is a conversation with an AI assistant. The assistant is helpful, creative, clever, and very friendly.\n" +
		"\nHuman: Hello, who are you?" +
		"\nAI: I am an AI created by OpenAI. How can I help you today?" +
		"\nHuman: "
	gptDefaultAIMessage = "How can I help you today?"
	gptPromptAI         = "\nAI: "
	gptPromptHuman      = "\nHuman: "
)

type dbMessage struct {
	ID        int
	UserID    int
	Username  string
	Text      string
	CreatedAt time.Time
}

func main() {
	apiKeyOpenAI := os.Getenv("API_KEY_OPENAPI")
	apiKeyTelegram := os.Getenv("API_KEY_TELEGRAM")
	userIDTelegram := os.Getenv("USER_ID_TELEGRAM")
	applicationDataRootDirPath := os.Getenv("APPLICATION_DATA_ROOT_DIR_PATH")
	databaseFilename := os.Getenv("DATABASE_FILENAME")
	sqlMigrationsDirPathRelative := os.Getenv("SQL_MIGRATIONS_PATH_RELATIVE")
	maxMessagesInHistoryStr := os.Getenv("MAX_MESSAGES_IN_HISTORY")
	maxTokensToGenerateStr := os.Getenv("MAX_TOKENS_TO_GENERATE")
	debugLogPromptsStr := os.Getenv("DEBUG_LOG_PROMPTS")

	ctxInit, ctxInitCancel := context.WithTimeout(context.Background(), initTimeout)
	defer ctxInitCancel()

	ensureNoError := func(err error, entiry string) {
		if err != nil {
			log.Panicf("failed to initialize %v: %v", entiry, err)
		}
	}

	// ==== Initialize the application ====

	log.Println("initializing")

	cwd, err := os.Getwd()
	ensureNoError(err, "current working directory")

	// ---- Parameters ----

	if applicationDataRootDirPath == "" {
		applicationDataRootDirPath = defaultApplicationDataRootDirPath
	}

	if sqlMigrationsDirPathRelative == "" {
		sqlMigrationsDirPathRelative = defaultSQLMigrationsDirPathRelative
	}

	maxMessagesInHistory := defaultMaxMessagesInHistory
	if maxMessagesInHistoryStr != "" {
		maxMessagesInHistory, err = strconv.Atoi(maxMessagesInHistoryStr)
		ensureNoError(err, "maximum number of messages in history")
	}

	maxTokensToGenerate := defaultMaxTokensToGenerate
	if maxTokensToGenerateStr != "" {
		maxTokensToGenerate, err = strconv.Atoi(maxTokensToGenerateStr)
		ensureNoError(err, "maximum number of tokens to generate")
	}

	debugLogPrompts := debugLogPromptsStr == "true"

	// ---- Database ----

	if databaseFilename == "" {
		databaseFilename = defaultDatabaseFilename
	}
	databaseFilePath := applicationDataRootDirPath + ps + databaseFilename
	sqlMigrationsDirPath := cwd + ps + sqlMigrationsDirPathRelative

	db, err := sql.Open(sqlDatabaseDriverName, databaseFilePath)
	ensureNoError(err, "SQLite database")
	defer db.Close()

	dbDriver, err := sqlite3.WithInstance(db, &sqlite3.Config{
		DatabaseName: sqlDatabaseDriverName,
	})
	ensureNoError(err, "SQLite driver for database migration")

	log.Println("run database migrations from", sqlMigrationsDirPath)

	dbMigrator, err := migrate.NewWithDatabaseInstance(
		"file://"+sqlMigrationsDirPath,
		sqlDatabaseDriverName,
		dbDriver,
	)
	ensureNoError(err, "SQLite database migrator")

	if err = dbMigrator.Up(); errors.Is(err, migrate.ErrNoChange) {
		err = nil
	}
	ensureNoError(err, "SQLite database schema")

	// ---- OpenAI API ----

	gptClient := gpt3.NewClient(apiKeyOpenAI)

	// ---- Telegram API ----

	bot, err := tgbotapi.NewBotAPI(apiKeyTelegram)
	ensureNoError(err, "Telegram bot API client")

	// Set up an update listener to receive incoming messages
	u := tgbotapi.NewUpdate(0)
	u.Timeout = telegramBotUpdaterTimeoutSeconds
	tgUpdates, err := bot.GetUpdatesChan(u)
	ensureNoError(err, "Telegram bot updates channel")

	// ==== Run the application ====

	log.Println("started")

	ctxRun, ctxRunCancel := context.WithCancel(context.Background())
	defer ctxRunCancel()

	// ---- Handle OS signals to shutdown gracefully ----

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("received signal '%v', terminating\n", sig)
		ctxRunCancel()
	}()

	go func(ctx context.Context) {
		<-ctx.Done()

		tgUpdates.Clear()
		bot.StopReceivingUpdates()
	}(ctxRun)

	// ---- Process incoming messages ----

	done := make(chan struct{})
	go processIncomingMessages(
		ctxRun,
		userIDTelegram,
		maxMessagesInHistory,
		maxTokensToGenerate,
		db,
		bot,
		gptClient,
		tgUpdates,
		debugLogPrompts,
		done,
	)

	_ = ctxInit

	// ---- Wait for shutdown ----

	<-ctxRun.Done()
	<-done
	log.Println("terminated")
}

func processIncomingMessages(
	ctx context.Context,
	userIDTelegram string,
	maxMessagesInHistory int,
	maxTokensToGenerate int,
	db *sql.DB,
	bot *tgbotapi.BotAPI,
	gptClient *gpt3.Client,
	tgUpdates tgbotapi.UpdatesChannel,
	debugLogPrompts bool,
	done chan<- struct{},
) {
	defer func() { close(done) }()

UPDATES:
	for {
		var update tgbotapi.Update
		select {
		case update = <-tgUpdates:
		case <-ctx.Done():
			break UPDATES
		}

		if update.Message == nil {
			continue
		}
		if strconv.FormatInt(int64(update.Message.From.ID), 10) != userIDTelegram {
			log.Println("rejecting message from unknown user", update.Message.From.ID)
			continue
		}

		if err := deleteOldMessages(ctx, db, maxMessagesInHistory); err != nil {
			log.Println("failed to delete old messages from the database:", err)
		}

		log.Printf("recieved new message with %d bytes\n", len(update.Message.Text))

		history, err := getAllMesssages(ctx, db)
		if err != nil {
			log.Println("failed to get conversation history from the database:", err)
			sendErrorMessage(bot, update, err)
			continue
		}

		if err := saveMessage(ctx, db, &dbMessage{
			UserID:    update.Message.From.ID,
			Username:  update.Message.From.UserName,
			Text:      update.Message.Text,
			CreatedAt: time.Now(),
		}); err != nil {
			log.Printf("failed to save incoming message to the database: %v\n", err)
			sendErrorMessage(bot, update, err)
			continue
		}

		prompt := buildPromptFromHistory(maxTokensToGenerate, history, update.Message.Text)

		if debugLogPrompts {
			log.Println("==== PROMPT:", prompt)
		}

		req := gpt3.CompletionRequest{
			Model:            gptModel,
			Prompt:           prompt,
			Temperature:      0.9,
			MaxTokens:        maxTokensToGenerate,
			TopP:             1,
			FrequencyPenalty: 0,
			PresencePenalty:  0.6,
			Stop:             []string{" Human:", " AI:"},
		}
		resp, err := gptClient.CreateCompletion(ctx, req)
		if err != nil {
			log.Println("failed to get response from GPT model:", err)
			sendErrorMessage(bot, update, err)
			continue
		}
		respText := resp.Choices[0].Text

		if err := saveMessage(ctx, db, &dbMessage{
			UserID:    0,
			Username:  "",
			Text:      respText,
			CreatedAt: time.Now(),
		}); err != nil {
			log.Printf("failed to save outgoing message to the database: %v\n", err)
			sendErrorMessage(bot, update, err)
			continue
		}

		msg := tgbotapi.NewMessage(update.Message.Chat.ID, respText)
		msg.ParseMode = tgbotapi.ModeMarkdown
		sendMessage(bot, msg)
	}
}

func buildPromptFromHistory(maxTokensToGenerate int, history []*dbMessage, humanMessage string) string {
	initial := gptContextInitial

	buf := new(strings.Builder)
	buf.WriteString(initial)

	rows := make([]string, 0, len(history))
	wantHumanMessage := true
	for _, msg := range history {
		if (wantHumanMessage && msg.UserID == 0) || (!wantHumanMessage && msg.UserID != 0) {
			// Skip messages that are out of "Human -> AI -> Human -> AI -> ..." order
			continue
		}

		row := msg.Text
		if wantHumanMessage {
			row += gptPromptAI
		} else {
			row += gptPromptHuman
		}
		rows = append(rows, row)

		wantHumanMessage = !wantHumanMessage
	}
	if !wantHumanMessage {
		// If last message in the prompt is Human message, add default AI message to the end
		rows = append(rows, gptDefaultAIMessage+gptPromptHuman)
	}
	rows = append(rows, humanMessage+gptPromptAI)

	// Delete older rows if total prompt length plus completion length exceed model limit
	for exceedsLimit(initial, rows, maxTokensToGenerate) && len(rows) >= 2 {
		// Remove two oldest rows (rows are ordered to be "Human -> AI -> ..." here)
		rows = rows[2:]
	}

	for _, row := range rows {
		buf.WriteString(row)
	}
	return buf.String()
}

func exceedsLimit(initial string, rows []string, maxTokensToGenerate int) bool {
	if len(rows) == 0 {
		return false
	}

	total := len(initial)
	for _, row := range rows {
		total += len(row)
	}
	total += maxTokensToGenerate

	return total > gptModelContextLengthMax
}

func sendErrorMessage(bot *tgbotapi.BotAPI, update tgbotapi.Update, err error) {
	msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("`Failed to process your request. ERROR: %v`", err))
	msg.ParseMode = tgbotapi.ModeMarkdown
	sendMessage(bot, msg)
}

func sendMessage(bot *tgbotapi.BotAPI, msg tgbotapi.MessageConfig) {
	if _, err := bot.Send(msg); err != nil {
		log.Println("failed to send a message:", err)
	} else {
		log.Printf("sent a message with %d bytes\n", len(msg.Text))
	}
}

func getAllMesssages(ctx context.Context, db *sql.DB) ([]*dbMessage, error) {
	const query = `
		SELECT id, user_id, username, message, created_at FROM chat_history ORDER BY created_at ASC
	`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query for all messages from the database: %w", err)
	}
	defer rows.Close()

	history := make([]*dbMessage, 0)
	for rows.Next() {

		msg := new(dbMessage)
		var msgCreatedAt string
		if err := rows.Scan(&msg.ID, &msg.UserID, &msg.Username, &msg.Text, &msgCreatedAt); err != nil {
			return nil, fmt.Errorf("failed to get message from the database: %w", err)
		}

		if msg.CreatedAt, err = time.Parse(databaseDateTimeLayout, msgCreatedAt); err != nil {
			return nil, fmt.Errorf("failed to parse datetime '%v' with layout '%v': %w", msgCreatedAt, databaseDateTimeLayout, err)
		}

		history = append(history, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to get messages from the database: %w", err)
	}
	return history, nil
}

func saveMessage(ctx context.Context, db *sql.DB, msg *dbMessage) error {
	const query = `
		INSERT INTO chat_history(user_id, username, message, created_at)
		VALUES(?, ?, ?, ?)
	`

	stmt, err := db.PrepareContext(ctx, query)
	if err != nil {
		return err
	}
	defer stmt.Close()

	if _, err = stmt.ExecContext(ctx, msg.UserID, msg.Username, msg.Text, msg.CreatedAt); err != nil {
		return err
	}

	return nil
}

func deleteOldMessages(ctx context.Context, db *sql.DB, maxMessages int) error {
	countRow := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM chat_history")

	var count int
	if err := countRow.Scan(&count); err != nil {
		return fmt.Errorf("failed to get message count from database: %v", err)
	}

	if count > maxMessages {
		oldMessageRow := db.QueryRowContext(ctx, "SELECT id FROM chat_history ORDER BY created_at DESC LIMIT 1 OFFSET ?", maxMessages)

		var oldMessageID int64
		if err := oldMessageRow.Scan(&oldMessageID); err != nil {
			return fmt.Errorf("failed to get old message ID from database: %v", err)
		}

		if _, err := db.ExecContext(ctx, "DELETE FROM chat_history WHERE id <= ?", oldMessageID); err != nil {
			return fmt.Errorf("failed to delete old messages from database: %v", err)
		}
	}
	return nil
}
