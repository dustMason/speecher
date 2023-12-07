package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
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

var (
	s *discordgo.Session

	commands = []*discordgo.ApplicationCommand{
		{
			Name:        "stest1",
			Description: "Command for demonstrating options",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "url",
					Description: "URL to read",
					Required:    true,
				},
			},
		},
	}

	commandHandlers = map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate){
		"stest1": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			options := i.ApplicationCommandData().Options
			optionMap := make(map[string]*discordgo.ApplicationCommandInteractionDataOption, len(options))
			for _, opt := range options {
				optionMap[opt.Name] = opt
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
					s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
						Content: "Something went wrong",
					})
					return
				}
				extracted := callExtractorAPI(option.StringValue())
				fmt.Println(extracted.Text)

				c := "Calling OpenAI TTS API..."
				_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &c})
				if err != nil {
					s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
						Content: "Something went wrong",
					})
					return
				}

				if len(extracted.Text) > 4095 {
					extracted.Text = extracted.Text[:4095]
				}
				callOpenAI(extracted.Text)

				// todo upload to s3 and return the URL here

				c = "Done!"
				_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &c})
				if err != nil {
					s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
						Content: "Something went wrong",
					})
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

	log.Println("Adding commands...")
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

func callExtractorAPI(url string) ExtractorResponse {
	apiKey := os.Getenv("EXTRACTOR_API_KEY")
	resp, _ := http.Get("https://extractorapi.com/api/v1/extractor/?apikey=" + apiKey + "&url=" + url)
	body, _ := io.ReadAll(resp.Body)
	var extractorResponse ExtractorResponse
	json.Unmarshal(body, &extractorResponse)
	return extractorResponse
}

func callOpenAI(text string) (string, error) {
	client := &http.Client{}
	reqBody, _ := json.Marshal(OpenAIRequest{
		Model: "tts-1",
		Input: text,
		Voice: "alloy",
	})
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/audio/speech", strings.NewReader(string(reqBody)))
	req.Header.Add("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	req.Header.Add("Content-Type", "application/json")
	resp, _ := client.Do(req)
	body, _ := io.ReadAll(resp.Body)

	// Generate a random string of 10 bytes and encode it as hexadecimal
	randBytes := make([]byte, 10)
	_, _ = rand.Read(randBytes)
	filename := "speech_" + hex.EncodeToString(randBytes) + ".mp3"

	err := os.WriteFile(filename, body, 0644)
	if err != nil {
		return "", err
	}
	return filename, nil
}

//func uploadToS3(filePath string) string {
//	file, _ := os.Open(filePath)
//	uploader := s3manager.NewUploader(session.New(&aws.Config{Region: aws.String("us-west-2")}))
//	result, _ := uploader.Upload(&s3manager.UploadInput{
//		Bucket: aws.String("<your-bucket-name>"),
//		Key:    aws.String(filePath),
//		Body:   file,
//	})
//	return result.Location
//}
