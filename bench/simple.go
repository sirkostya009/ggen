// NoAlloc bench family (simple_test.go): the wide, flat, scalar-only Account
// record whose bytes-path decode makes zero allocations.

//go:generate ../ggen $GOFILE
package bench

// Account is the zero-allocation parse target (BenchmarkNoAlloc_Unmarshal):
// a wide denormalized record — profile, address, employer, settings —
// flattened into one object. Free of every kind that forces a decode alloc
// (no slices/maps/pointers/any/RawMessage), so a full decode makes ZERO
// allocations: strings alias the input, nested structs decode in place.
//
//ggen:generate
type Account struct {
	ID          uint64  `json:"id"`
	Username    string  `json:"username"`
	Email       string  `json:"email"`
	FirstName   string  `json:"firstName"`
	LastName    string  `json:"lastName"`
	MiddleName  string  `json:"middleName"`
	DisplayName string  `json:"displayName"`
	Phone       string  `json:"phone"`
	Age         uint8   `json:"age"`
	Verified    bool    `json:"verified"`
	Active      bool    `json:"active"`
	Premium     bool    `json:"premium"`
	Suspended   bool    `json:"suspended"`
	Deleted     bool    `json:"deleted"`
	Balance     float64 `json:"balance"`
	Reputation  int32   `json:"reputation"`
	TrustScore  float64 `json:"trustScore"`

	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
	LastLogin    int64  `json:"lastLogin"`
	LoginCount   uint32 `json:"loginCount"`
	FailedLogins uint16 `json:"failedLogins"`

	Bio       string `json:"bio"`
	AvatarURL string `json:"avatarUrl"`
	BannerURL string `json:"bannerUrl"`
	Locale    string `json:"locale"`

	FollowerCount  int `json:"followerCount"`
	FollowingCount int `json:"followingCount"`
	PostCount      int `json:"postCount"`

	StorageUsed      int64 `json:"storageUsed"`
	StorageQuota     int64 `json:"storageQuota"`
	TwoFactorEnabled bool  `json:"twoFactorEnabled"`

	Address     PostalAddress `json:"address"`
	Company     Company       `json:"company"`
	Preferences Preferences   `json:"preferences"`
}

//ggen:generate
type PostalAddress struct {
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	City       string `json:"city"`
	State      string `json:"state"`
	PostalCode string `json:"postalCode"`
	Country    string `json:"country"`
	Geo        Geo    `json:"geo"`
}

//ggen:generate
type Geo struct {
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	Altitude float64 `json:"altitude"`
	Accuracy float32 `json:"accuracy"`
}

//ggen:generate
type Company struct {
	Name       string `json:"name"`
	Department string `json:"department"`
	Title      string `json:"title"`
	EmployeeID string `json:"employeeId"`
	Headcount  int    `json:"headcount"`
	Founded    int16  `json:"founded"`
	IsPublic   bool   `json:"isPublic"`
}

//ggen:generate
type Preferences struct {
	Theme              string `json:"theme"`
	Language           string `json:"language"`
	Timezone           string `json:"timezone"`
	Currency           string `json:"currency"`
	EmailNotifications bool   `json:"emailNotifications"`
	PushNotifications  bool   `json:"pushNotifications"`
	SMSNotifications   bool   `json:"smsNotifications"`
	ItemsPerPage       uint8  `json:"itemsPerPage"`
	AutoSave           bool   `json:"autoSave"`
	BetaFeatures       bool   `json:"betaFeatures"`
}

// Copy* mirror the Account family under -copy (`//ggen:generate copy`), like
// CopyNode mirrors Node: every retained string is copied out of the input
// instead of aliased. Wire-identical to Account, so the `ggen_copy` NoAlloc
// row decodes the same AccountPayload. Geo is reused as-is (no strings).
//
//ggen:generate copy
type CopyAccount struct {
	ID          uint64  `json:"id"`
	Username    string  `json:"username"`
	Email       string  `json:"email"`
	FirstName   string  `json:"firstName"`
	LastName    string  `json:"lastName"`
	MiddleName  string  `json:"middleName"`
	DisplayName string  `json:"displayName"`
	Phone       string  `json:"phone"`
	Age         uint8   `json:"age"`
	Verified    bool    `json:"verified"`
	Active      bool    `json:"active"`
	Premium     bool    `json:"premium"`
	Suspended   bool    `json:"suspended"`
	Deleted     bool    `json:"deleted"`
	Balance     float64 `json:"balance"`
	Reputation  int32   `json:"reputation"`
	TrustScore  float64 `json:"trustScore"`

	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
	LastLogin    int64  `json:"lastLogin"`
	LoginCount   uint32 `json:"loginCount"`
	FailedLogins uint16 `json:"failedLogins"`

	Bio       string `json:"bio"`
	AvatarURL string `json:"avatarUrl"`
	BannerURL string `json:"bannerUrl"`
	Locale    string `json:"locale"`

	FollowerCount  int `json:"followerCount"`
	FollowingCount int `json:"followingCount"`
	PostCount      int `json:"postCount"`

	StorageUsed      int64 `json:"storageUsed"`
	StorageQuota     int64 `json:"storageQuota"`
	TwoFactorEnabled bool  `json:"twoFactorEnabled"`

	Address     CopyPostalAddress `json:"address"`
	Company     CopyCompany       `json:"company"`
	Preferences CopyPreferences   `json:"preferences"`
}

//ggen:generate copy
type CopyPostalAddress struct {
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	City       string `json:"city"`
	State      string `json:"state"`
	PostalCode string `json:"postalCode"`
	Country    string `json:"country"`
	Geo        Geo    `json:"geo"`
}

//ggen:generate copy
type CopyCompany struct {
	Name       string `json:"name"`
	Department string `json:"department"`
	Title      string `json:"title"`
	EmployeeID string `json:"employeeId"`
	Headcount  int    `json:"headcount"`
	Founded    int16  `json:"founded"`
	IsPublic   bool   `json:"isPublic"`
}

//ggen:generate copy
type CopyPreferences struct {
	Theme              string `json:"theme"`
	Language           string `json:"language"`
	Timezone           string `json:"timezone"`
	Currency           string `json:"currency"`
	EmailNotifications bool   `json:"emailNotifications"`
	PushNotifications  bool   `json:"pushNotifications"`
	SMSNotifications   bool   `json:"smsNotifications"`
	ItemsPerPage       uint8  `json:"itemsPerPage"`
	AutoSave           bool   `json:"autoSave"`
	BetaFeatures       bool   `json:"betaFeatures"`
}

// Easy* mirror the Account family for the easyjson rows, kept on separate
// types so easyjson's methods don't leak into the jsonv2/sonic rows. Same
// wire shape — see "easyjson method leakage" in bench/CLAUDE.md.
//
//easyjson:json
type EasyAccount struct {
	ID          uint64  `json:"id"`
	Username    string  `json:"username"`
	Email       string  `json:"email"`
	FirstName   string  `json:"firstName"`
	LastName    string  `json:"lastName"`
	MiddleName  string  `json:"middleName"`
	DisplayName string  `json:"displayName"`
	Phone       string  `json:"phone"`
	Age         uint8   `json:"age"`
	Verified    bool    `json:"verified"`
	Active      bool    `json:"active"`
	Premium     bool    `json:"premium"`
	Suspended   bool    `json:"suspended"`
	Deleted     bool    `json:"deleted"`
	Balance     float64 `json:"balance"`
	Reputation  int32   `json:"reputation"`
	TrustScore  float64 `json:"trustScore"`

	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
	LastLogin    int64  `json:"lastLogin"`
	LoginCount   uint32 `json:"loginCount"`
	FailedLogins uint16 `json:"failedLogins"`

	Bio       string `json:"bio"`
	AvatarURL string `json:"avatarUrl"`
	BannerURL string `json:"bannerUrl"`
	Locale    string `json:"locale"`

	FollowerCount  int `json:"followerCount"`
	FollowingCount int `json:"followingCount"`
	PostCount      int `json:"postCount"`

	StorageUsed      int64 `json:"storageUsed"`
	StorageQuota     int64 `json:"storageQuota"`
	TwoFactorEnabled bool  `json:"twoFactorEnabled"`

	Address     EasyPostalAddress `json:"address"`
	Company     EasyCompany       `json:"company"`
	Preferences EasyPreferences   `json:"preferences"`
}

//easyjson:json
type EasyPostalAddress struct {
	Line1      string  `json:"line1"`
	Line2      string  `json:"line2"`
	City       string  `json:"city"`
	State      string  `json:"state"`
	PostalCode string  `json:"postalCode"`
	Country    string  `json:"country"`
	Geo        EasyGeo `json:"geo"`
}

//easyjson:json
type EasyGeo struct {
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	Altitude float64 `json:"altitude"`
	Accuracy float32 `json:"accuracy"`
}

//easyjson:json
type EasyCompany struct {
	Name       string `json:"name"`
	Department string `json:"department"`
	Title      string `json:"title"`
	EmployeeID string `json:"employeeId"`
	Headcount  int    `json:"headcount"`
	Founded    int16  `json:"founded"`
	IsPublic   bool   `json:"isPublic"`
}

//easyjson:json
type EasyPreferences struct {
	Theme              string `json:"theme"`
	Language           string `json:"language"`
	Timezone           string `json:"timezone"`
	Currency           string `json:"currency"`
	EmailNotifications bool   `json:"emailNotifications"`
	PushNotifications  bool   `json:"pushNotifications"`
	SMSNotifications   bool   `json:"smsNotifications"`
	ItemsPerPage       uint8  `json:"itemsPerPage"`
	AutoSave           bool   `json:"autoSave"`
	BetaFeatures       bool   `json:"betaFeatures"`
}

// AccountValue is a representative populated record; AccountPayload is its
// marshalled JSON, built once at init.
var (
	AccountValue   Account
	AccountPayload []byte
)

func init() {
	AccountValue = Account{
		ID:          9876543210,
		Username:    "аліса.андерсон",
		Email:       "аліса.андерсон@приклад-корпорація.укр",
		FirstName:   "Аліса",
		LastName:    "Андерсон",
		MiddleName:  "Маргарита",
		DisplayName: "アリサ・アンダーソン 🦊",
		Phone:       "+1-415-555-0173",
		Age:         34,
		Verified:    true,
		Active:      true,
		Premium:     true,
		Suspended:   false,
		Deleted:     false,
		Balance:     12489.57,
		Reputation:  84213,
		TrustScore:  98.6,

		CreatedAt:    1593561600,
		UpdatedAt:    1718668800,
		LastLogin:    1718712345,
		LoginCount:   4821,
		FailedLogins: 3,

		// Long multilingual non-ASCII bodies — exercise the UTF-8 string
		// scan (multi-byte runes, no escapes) over large fields.
		Bio: "Провідна інженерка розподілених систем. ‹分散システムの主任エンジニア›. " +
			"Кохаю каву ☕, біг по стежках 🏃 та маю давню образу на необмежені черги. " +
			"Mes opinions sont porteuses — μην εμπιστεύεσαι ουρές χωρίς όριο. " +
			"Строю надійні конвеєри даних, пишу про backpressure, спостережуваність і те, " +
			"чому «просто додай ще один воркер» — це не стратегія. 🧵📊🛰️ " +
			"In früheren Leben: компілятори, ядра, та забагато YAML. 日々精進。",
		AvatarURL: "https://кеш.приклад-корпорація.укр/аватари/аліса-андерсон/" +
			"профіль_512x512.webp?версія=42&підпис=a1b2c3d4e5f6&регіон=eu-central&тема=темна",
		BannerURL: "https://кеш.приклад-корпорація.укр/банери/аліса-андерсон/" +
			"обкладинка_1500x500.webp?версія=17&підпис=f6e5d4c3b2a1&регіон=eu-central&палітра=ніч",
		Locale: "uk-UA",

		FollowerCount:  18342,
		FollowingCount: 312,
		PostCount:      2774,

		StorageUsed:      8734092123,
		StorageQuota:     53687091200,
		TwoFactorEnabled: true,

		Address: PostalAddress{
			Line1:      "вулиця Хрещатик, буд. 22",
			Line2:      "офіс 4200, поверх 12",
			City:       "Київ",
			State:      "Київська область",
			PostalCode: "01001",
			Country:    "Україна 🇺🇦",
			Geo: Geo{
				Lat:      50.450100,
				Lng:      30.523400,
				Altitude: 179.5,
				Accuracy: 4.75,
			},
		},
		Company: Company{
			Name:       "Приклад Корпорація Інтернешнл «Хмара»",
			Department: "Платформна інфраструктура",
			Title:      "Головна інженерка з програмного забезпечення",
			EmployeeID: "СПІВ-0000-4821",
			Headcount:  18750,
			Founded:    1998,
			IsPublic:   true,
		},
		Preferences: Preferences{
			Theme:              "темна",
			Language:           "українська",
			Timezone:           "Europe/Kyiv",
			Currency:           "₴ UAH",
			EmailNotifications: true,
			PushNotifications:  false,
			SMSNotifications:   false,
			ItemsPerPage:       50,
			AutoSave:           true,
			BetaFeatures:       true,
		},
	}

	AccountPayload = mustMarshal(AccountValue)
}
