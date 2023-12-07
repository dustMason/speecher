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

// todo write a goroutine that crawls the files directory and deletes files older than 1 hour. it should run hourly

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
			},
			// todo add option for voice
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

				randBytes := make([]byte, 10)
				_, _ = rand.Read(randBytes)
				pathPrefix := os.Getenv("FILES_PATH")
				filename := pathPrefix + "speech_" + hex.EncodeToString(randBytes) + ".mp3"

				const maxLen = 4095 // TTS API can do max 4096 characters
				file, err := os.Create(filename)
				if err != nil {
					doError(err)
					return
				}
				defer file.Close()
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

					c := fmt.Sprintf("Calling OpenAI TTS API go get audio for '%s' (chunk %d)", extracted.Title, n)
					_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &c})
					if err != nil {
						doError(err)
						return
					}

					body := callOpenAI(chunk)
					_, err = io.Copy(file, body)
					if err != nil {
						doError(err)
						return
					}
					_ = body.Close()
					n++
				}

				// todo c should be a link to download the file, which means we also need to operate a webserver...
				c := "Done!"
				_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &c})
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

func callOpenAI(text string) io.ReadCloser {
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
