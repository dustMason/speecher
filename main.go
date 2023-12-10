package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/bwmarrin/discordgo"
	"io"
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
	s *discordgo.Session

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
					doError(err)
					return
				}
				extracted := callExtractorAPI(option.StringValue())
				preview := extracted.Text
				if len(preview) > 100 {
					preview = preview[:100]
				}
				fmt.Printf("Extracted text (first 100 characters): %s\n", preview)

				tempFile, err := os.CreateTemp("", "speech_*.mp3")
				if err != nil {
					doError(err)
					return
				}
				defer tempFile.Close()

				const maxLen = 4095 // TTS API can do max 4096 characters
				n := 1
				for len(extracted.Text) > 0 {
					chunkLen := len(extracted.Text)
					if chunkLen > maxLen {
						chunkLen = maxLen
						// Find the last period in the current chunk
						for i := chunkLen; i >= 0; i-- {
							if extracted.Text[i] == '.' {
								chunkLen = i + 1
								break
							}
						}
					}

					chunk := extracted.Text[:chunkLen]
					extracted.Text = extracted.Text[chunkLen:]

					c := fmt.Sprintf("Calling OpenAI TTS API to get audio for '%s' (chunk %d)", extracted.Title, n)
					_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &c})
					if err != nil {
						doError(err)
						return
					}

					voice := "alloy"
					if v, ok := optionMap["voice"]; ok {
						voice = v.StringValue()
					}

					body := callOpenAI(chunk, voice)
					_, err = io.Copy(tempFile, body)
					if err != nil {
						doError(err)
						return
					}
					_ = body.Close()
					n++
				}

				_, err = tempFile.Seek(0, 0)
				if err != nil {
					doError(err)
					return
				}
				mp3Url, uerr := uploadToS3(tempFile)
				if uerr != nil {
					doError(uerr)
					return
				}

				finalMessage := fmt.Sprintf("Here's the audio for '%s' (%s)\n\n%s", extracted.Title, url, mp3Url)

				_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &finalMessage})
				if err != nil {
					doError(err)
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

func callOpenAI(text, voice string) io.ReadCloser {
	client := &http.Client{}
	reqBody, _ := json.Marshal(OpenAIRequest{
		Model: "tts-1",
		Input: text,
		Voice: voice,
	})
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/audio/speech", strings.NewReader(string(reqBody)))
	req.Header.Add("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	req.Header.Add("Content-Type", "application/json")
	fmt.Println("Calling OpenAI TTS API with len(text) =", len(text))
	start := time.Now()
	resp, _ := client.Do(req)
	fmt.Println("OpenAI TTS API call took", time.Since(start))
	return resp.Body
}

func callExtractorAPI(url string) ExtractorResponse {
	apiKey := os.Getenv("EXTRACTOR_API_KEY")
	resp, _ := http.Get("https://extractorapi.com/api/v1/extractor/?apikey=" + apiKey + "&url=" + url)
	body, _ := io.ReadAll(resp.Body)
	var extractorResponse ExtractorResponse
	json.Unmarshal(body, &extractorResponse)
	return extractorResponse
}

func uploadToS3(f *os.File) (string, error) {
	sess, err := session.NewSession(&aws.Config{Region: aws.String("us-east-1")})
	if err != nil {
		return "", err
	}
	uploader := s3manager.NewUploader(sess)
	randomBytes := make([]byte, 4) // 4 bytes * 2 for hex encoding = 8 characters
	_, err = rand.Read(randomBytes)
	if err != nil {
		return "", err
	}
	randomString := hex.EncodeToString(randomBytes)
	fileName := time.Now().Format("2006-01-02") + "_" + randomString + ".mp3"
	fmt.Println("Uploading to S3 as", fileName)
	start := time.Now()
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String("op1fun"),
		Key:    aws.String("speecher/" + fileName),
		Body:   f,
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload file, %v", err)
	}
	fmt.Println("Upload to S3 took", time.Since(start))
	url := "https://op1fun.s3.us-east-1.amazonaws.com/speecher/" + fileName
	return url, nil
}
