# Use an official Golang runtime as a parent image
FROM golang:1.19

# Set environment variables
ENV API_KEY_OPENAPI=xxxxxx \
    API_KEY_TELEGRAM=xxxxxx \
    USER_ID_TELEGRAM=xxxxxx \
    APPLICATION_DATA_ROOT_DIR_PATH=/data \
    DATABASE_FILENAME=db.sqlite \
    SQL_MIGRATIONS_PATH_RELATIVE=database/migrations/ \
    MAX_MESSAGES_IN_HISTORY=101 \
    MAX_TOKENS_TO_GENERATE=301 \
    DEBUG_LOG_PROMPTS=false

# Set the working directory to /app
WORKDIR /app

# Copy the current directory contents into the container at /app
COPY . /app

# Build the Go app
RUN go build -o main ./cmd

# Command to run the executable
CMD ["./main"]
