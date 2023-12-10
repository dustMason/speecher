package main

import (
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/bwmarrin/discordgo"
	"github.com/hyacinthus/mp3join"
	"golang.org/x/sync/errgroup"
	"io"
	"math/rand"
	"regexp"
	"sync"

	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

type ExtractorResponse struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

type OpenAIRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
	Voice string `json:"voice"`
}

const commandName = "speak"

var (
	s        *discordgo.Session
	commands = []*discordgo.ApplicationCommand{
		{
			Name:        commandName,
			Description: "Grab the contents of a webpage and return an mp3 of the text being read",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "url",
					Description: "URL to read",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "voice",
					Description: "Voice to use",
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Alloy (default)", Value: "alloy"},
						{Name: "Echo", Value: "echo"},
						{Name: "Fable", Value: "fable"},
						{Name: "Onyx", Value: "onyx"},
						{Name: "Nova", Value: "nova"},
						{Name: "Shimmer", Value: "shimmer"},
					},
				},
			},
		},
	}

	commandHandlers = map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate){
		commandName: func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			options := i.ApplicationCommandData().Options
			optionMap := make(map[string]*discordgo.ApplicationCommandInteractionDataOption, len(options))
			for _, opt := range options {
				optionMap[opt.Name] = opt
			}

			doError := func(e error) {
				fmt.Printf("error: %+v\n", e)
				_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
					Content: "Something went wrong: " + e.Error(),
				})
			}

			if option, ok := optionMap["url"]; ok {
				url := option.StringValue()
				err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "Requesting contents of " + url,
					},
				})
				if err != nil {
					fmt.Printf("error: %+v\n", err)
					return
				}
				extracted, err := callExtractorAPI(option.StringValue())
				if err != nil {
					doError(err)
					return
				}

				chunks := textChunks(extracted.Text, 4095)
				c := fmt.Sprintf("Calling OpenAI TTS API to get audio for '%s' (%d chunks)", extracted.Title, len(chunks))
				_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &c})
				if err != nil {
					fmt.Printf("error: %+v\n", err)
					return
				}

				voice := "alloy"
				if v, ok := optionMap["voice"]; ok {
					voice = v.StringValue()
				}
				r, err := getAudioForChunks(chunks, voice)
				if err != nil {
					doError(err)
					return
				}

				mp3Url, uerr := uploadToS3(r, extracted.Title, voice)
				if uerr != nil {
					doError(uerr)
					return
				}

				finalMessage := fmt.Sprintf("Here's the audio for '%s' (%s)\n\n%s", extracted.Title, url, mp3Url)
				_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &finalMessage})
				if err != nil {
					fmt.Printf("error: %+v\n", err)
					return
				}
			}
		},
	}
)

func init() {
	var err error
	token := os.Getenv("DISCORD_TOKEN")
	s, err = discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("Invalid bot parameters: %v", err)
	}
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if h, ok := commandHandlers[i.ApplicationCommandData().Name]; ok {
			h(s, i)
		}
	})
}

func main() {
	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("Logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)
	})
	err := s.Open()
	if err != nil {
		log.Fatalf("Cannot open the session: %v", err)
	}
	guildID := os.Getenv("GUILD_ID")
	registeredCommands := make([]*discordgo.ApplicationCommand, len(commands))
	for i, v := range commands {
		cmd, err := s.ApplicationCommandCreate(s.State.User.ID, guildID, v)
		if err != nil {
			log.Panicf("Cannot create '%v' command: %v", v.Name, err)
		}
		registeredCommands[i] = cmd
	}
	defer s.Close()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	log.Println("Press Ctrl+C to exit")
	<-stop
}

func getAudioForChunks(chunks []string, voice string) (io.Reader, error) {
	lock := sync.Mutex{}
	tempFiles := make([]*os.File, len(chunks))
	var g errgroup.Group
	for ii, chunk := range chunks {
		ii, chunk := ii, chunk // https://golang.org/doc/faq#closures_and_goroutines
		g.Go(func() error {
			body := getAudioForChunk(chunk, voice)
			tempFile, err := os.CreateTemp("", "speech_*.mp3")
			if err != nil {
				return err
			}
			start := time.Now()
			_, err = io.Copy(tempFile, body)
			if err != nil {
				return err
			}
			_ = body.Close()
			lock.Lock()
			tempFiles[ii] = tempFile
			lock.Unlock()
			fmt.Println("Writing to temp file took", time.Since(start))
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	joiner := mp3join.New()
	for _, tempFile := range tempFiles {
		_, err := tempFile.Seek(0, 0)
		if err != nil {
			return nil, err
		}
		err = joiner.Append(tempFile)
		defer tempFile.Close()
		if err != nil {
			return nil, err
		}
	}
	return joiner.Reader(), nil
}

func textChunks(input string, chunkSize int) []string {
	var chunks []string
	for len(input) > 0 {
		chunkLen := len(input)
		if chunkLen > chunkSize {
			chunkLen = chunkSize
			// Find the last period in the current chunk
			for i := chunkLen; i >= 0; i-- {
				if input[i] == '.' {
					chunkLen = i + 1
					break
				}
			}
		}
		chunk := input[:chunkLen]
		input = input[chunkLen:]
		chunks = append(chunks, chunk)
	}
	return chunks
}

func getAudioForChunk(text, voice string) io.ReadCloser {
	client := &http.Client{}
	reqBody, _ := json.Marshal(OpenAIRequest{Model: "tts-1", Input: text, Voice: voice})
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/audio/speech", strings.NewReader(string(reqBody)))
	req.Header.Add("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	req.Header.Add("Content-Type", "application/json")
	fmt.Println("Calling OpenAI TTS API with len(text) =", len(text))
	start := time.Now()
	resp, _ := client.Do(req)
	fmt.Println("Streaming response from OpenAI after", time.Since(start))
	return resp.Body
}

func callExtractorAPI(url string) (ExtractorResponse, error) {
	apiKey := os.Getenv("EXTRACTOR_API_KEY")
	resp, err := http.Get("https://extractorapi.com/api/v1/extractor/?apikey=" + apiKey + "&url=" + url)
	if err != nil {
		return ExtractorResponse{}, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ExtractorResponse{}, err
	}
	var extractorResponse ExtractorResponse
	err = json.Unmarshal(body, &extractorResponse)
	if err != nil {
		return ExtractorResponse{}, err
	}
	preview := extractorResponse.Text
	if len(preview) > 100 {
		preview = preview[:100]
	}
	fmt.Printf("Extracted text (first 100 characters): %s\n", preview)
	return extractorResponse, nil
}

func uploadToS3(f io.Reader, title, voice string) (string, error) {
	sess, err := session.NewSession(&aws.Config{Region: aws.String(os.Getenv("AWS_REGION"))})
	if err != nil {
		return "", err
	}
	uploader := s3manager.NewUploader(sess)
	reg, err := regexp.Compile("[^a-zA-Z0-9]+")
	if err != nil {
		return "", err
	}
	safeTitle := reg.ReplaceAllString(title, "-")
	if len(safeTitle) > 42 {
		safeTitle = safeTitle[:42]
	}
	r := rand.Intn(9000) + 1000
	fileName := fmt.Sprintf("%s-%s-%s-%04d.mp3", time.Now().Format("2006-01-02"), safeTitle, voice, r)
	fmt.Println("Uploading to S3 as", fileName)
	start := time.Now()
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(os.Getenv("S3_BUCKET")),
		Key:    aws.String("speecher/" + fileName),
		Body:   f,
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload file, %v", err)
	}
	fmt.Println("Upload to S3 took", time.Since(start))
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/speecher/%s", os.Getenv("S3_BUCKET"), os.Getenv("AWS_REGION"), fileName), nil
}
