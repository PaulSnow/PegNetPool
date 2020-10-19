package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/FactomWyomingEntity/prosper-pool/config"
	"github.com/FactomWyomingEntity/prosper-pool/exit"
	"github.com/FactomWyomingEntity/prosper-pool/loghelp"
	"github.com/FactomWyomingEntity/prosper-pool/profile"
	"github.com/FactomWyomingEntity/prosper-pool/stratum"
	"github.com/pegnet/pegnet/modules/factoidaddress"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/crypto/ssh/terminal"
)

const (
	// Config Stuff
	ConfigHost           = "pool.host"
	ConfigNumGoRountines = "miner.threads"
	ConfigUserName       = "miner.username"
	ConfigMinerName      = "miner.minerid"
)

var rxEmail = regexp.MustCompile("^[a-zA-Z0-9.!#$%&'*+\\/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$")

func init() {
	rootCmd.PersistentFlags().StringP("config", "c", "", "config path location")
	rootCmd.PersistentFlags().String("log", "info", "Set the logger level (trace, debug, info, warn, error, or fatal)")
	rootCmd.PersistentFlags().Bool("profile", false, "Turn on profiling")

	rootCmd.Flags().Int("fake", -1, "Set a fake hash-rate")
	rootCmd.Flags().MarkHidden("fake")

	rootCmd.Flags().Bool("seq", false, "Use sequential vs batch hashing")
	rootCmd.Flags().Int("bs", 256, "Batch size for parallel hashing")

	// Should be set by the user
	rootCmd.Flags().StringP("user", "u", "", "Username to log into the mining pool")
	rootCmd.Flags().StringP("minerid", "m", GenerateMinerID(), "Minerid should be unique per mining machine")
	rootCmd.Flags().StringP("invitecode", "i", "", "Invite code for initial user registration")
	rootCmd.Flags().StringP("payoutaddress", "a", "", "Address to receive payments at (for initial user registration)")

	rootCmd.Flags().BoolP("password", "p", false, "Enable password prompt for user registration")

	// Defaults
	rootCmd.Flags().StringP("poolhost", "s", "localhost:1234", "URL to connect to the pool")
	rootCmd.Flags().IntP("miners", "t", runtime.NumCPU(), "Number of mining threads")

	rootCmd.AddCommand(properties)
}

// Pool entry point
func main() {
	Execute()
}

// Execute is cobra's entry point
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:     "prosper-miner",
	Short:   "Launch miner to communicate with the prosper mining pool.",
	PreRunE: OpenConfig,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		exit.GlobalExitHandler.AddCancel(cancel)
		keyboardReader := bufio.NewReader(os.Stdin)

		username, minerid := viper.GetString(ConfigUserName), viper.GetString(ConfigMinerName)

		password := ""

		promptForPassword, _ := cmd.Flags().GetBool("password")
		invitecode, _ := cmd.Flags().GetString("invitecode")
		payoutaddress, _ := cmd.Flags().GetString("payoutaddress")

		if promptForPassword {
			fmt.Print("Enter Password: ")
			bytePassword, err := terminal.ReadPassword(int(syscall.Stdin))
			if err != nil {
				log.Error("Problem with password parsing")
				return
			}
			password = strings.TrimSpace(string(bytePassword))

			fmt.Printf("\nConfirm Password: ")
			bytePasswordConfirmation, err := terminal.ReadPassword(int(syscall.Stdin))
			if err != nil {
				log.Error("Problem with password confirmation parsing")
				return
			}
			passwordConfirmation := strings.TrimSpace(string(bytePasswordConfirmation))
			fmt.Printf("\n")
			if strings.Compare(password, passwordConfirmation) != 0 {
				log.Error("Error: password doesn't match")
				return
			}

			err = factoidaddress.Valid(payoutaddress)
			if err != nil {
				fmt.Printf("%s is not a valid FA address: %s", payoutaddress, err.Error())
				os.Exit(1)
			}
		}

		if len(username) > 254 || !rxEmail.MatchString(username) {
			log.Error("Username must be a valid email address")
			return
		}

		client, err := stratum.NewClient(username, minerid, password, invitecode, payoutaddress, config.CompiledInVersion)
		if err != nil {
			panic(err)
		}

		miners := viper.GetInt(ConfigNumGoRountines)
		client.InitMiners(miners)
		fake, _ := cmd.Flags().GetInt("fake")
		if fake > 0 {
			log.Errorf("Fake hashing is disabled unless you modify he source")
			os.Exit(1)
			log.Warnf("!!FAKE MINING ENABLED!!")
			log.Warnf("All hashes are invalid. Rate is set at %d/s per core", fake)
			client.SetFakeHashRate(fake)
		}

		if seq, _ := cmd.Flags().GetBool("seq"); seq {
			log.Infof("Using sequential hashing method")
			client.RunMiners(ctx)
		} else {
			batchsize, err := cmd.Flags().GetInt("bs")
			if err != nil {
				panic(err)
			}
			log.WithFields(log.Fields{"batchsize": batchsize}).Infof("Using parallel hashing method")
			client.RunMinersBatch(ctx, batchsize)
		}

		// TODO: Add version number
		log.Infof("Initiated Prosper Miner")
		log.Infof("Username: %s, MinerID: %s", username, minerid)
		log.Infof("Using %d threads", miners)

		exit.GlobalExitHandler.AddExit(func() error {
			return client.Close()
		})

		err = client.Connect(viper.GetString(ConfigHost))
		if err != nil {
			panic(err)
		}

		client.Handshake()

		go func() {
			for {
				userCommand, _ := keyboardReader.ReadString('\n')
				words := strings.Fields(userCommand)
				if len(words) > 0 {
					switch words[0] {
					case "total":
						fmt.Printf("Total submit %d\n", client.TotalSuccesses())
					case "getopr":
						if len(words) > 1 {
							client.GetOPRHash(words[1])
						}
					case "suggesttarget":
						if len(words) > 1 {
							client.SuggestTarget(words[1])
						}
					default:
						fmt.Println("Client command not supported: ", words[0])
					}
				}
			}
		}()

		client.Listen(ctx)
	},
}

func OpenConfig(cmd *cobra.Command, args []string) error {
	initLogger(cmd)
	closeHandle()

	configPath, _ := cmd.Flags().GetString("config")
	configCustom := true
	if configPath == "" {
		if runtime.GOOS == "windows" {
			u, err := user.Current()
			if err == nil {
				_ = os.Setenv("HOME", u.HomeDir)
			}
		}
		configPath = "$HOME/.prosper/prosper-miner.toml" // Default
		configCustom = false
	}

	if pro, _ := cmd.Flags().GetBool("profile"); pro {
		go profile.StartProfiler(false, 6050) // Only localhost, on 6050
	}

	path := os.ExpandEnv(configPath)

	dir := filepath.Dir(path)
	name := filepath.Base(path)
	viper.AddConfigPath(dir)

	ext := filepath.Ext(name)
	viper.SetConfigName(strings.TrimSuffix(name, ext))

	info, err := os.Stat(path)
	exists := info != nil && !os.IsNotExist(err)

	// Set default config values
	SetDefaults(cmd)

	// If it does not exist, and not user specified, we will make it
	if !exists && !configCustom {
		if u, _ := cmd.Flags().GetString("user"); u == "" {
			return fmt.Errorf("no config found, username MUST be specified with -u <username>")
		}

		log.Infof("Writing config to file at %s", path)
		// Make the config
		dir := filepath.Dir(path)
		err := os.MkdirAll(dir, 0777)
		if err != nil {
			return fmt.Errorf("failed to make config path: %s", err.Error())
		}

		err = viper.WriteConfigAs(path)
		if err != nil {
			return fmt.Errorf("failed to write config: %s", err.Error())
		}
	} else if !exists && configCustom {
		return fmt.Errorf("error loading custom config path: %s", err.Error())
	} else {
		log.Infof("Using existing config")
	}

	// Read the config
	return viper.ReadInConfig()
}

func SetDefaults(cmd *cobra.Command) {
	_ = viper.BindPFlag(ConfigHost, cmd.Flags().Lookup("poolhost"))
	_ = viper.BindPFlag(ConfigNumGoRountines, cmd.Flags().Lookup("miners"))
	_ = viper.BindPFlag(ConfigUserName, cmd.Flags().Lookup("user"))
	_ = viper.BindPFlag(ConfigMinerName, cmd.Flags().Lookup("minerid"))
}

// GenerateMinerID has to be random
func GenerateMinerID() string {
	return NewRandomName(time.Now().UnixNano()).Haikunate()
}

func closeHandle() {
	// Catch ctl+c
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	go func() {
		<-signalChan
		log.Info("Gracefully closing")
		exit.GlobalExitHandler.Close()

		log.Info("closing application")
		// If something is hanging, we have to kill it
		os.Exit(0)
	}()
}

func initLogger(cmd *cobra.Command) {
	logLvl, _ := cmd.Flags().GetString("log")
	switch strings.ToLower(logLvl) {
	case "trace":
		log.SetLevel(log.TraceLevel)
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	case "fatal":
		log.SetLevel(log.FatalLevel)
	}

	log.StandardLogger().Hooks.Add(&loghelp.ContextHook{})
}
