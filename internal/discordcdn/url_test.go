package discordcdn

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttachmentURL(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://cdn.discordapp.com/attachments/1/2/file.txt?ex=one&hm=signed",
		"https://media.discordapp.net/attachments/1/2/file.png",
	} {
		require.Equal(t, raw, AttachmentURL(raw))
	}
	for _, raw := range []string{
		"http://cdn.discordapp.com/attachments/1/2/file.txt",
		"https://user@cdn.discordapp.com/attachments/1/2/file.txt",
		"https://cdn.discordapp.com:443/attachments/1/2/file.txt",
		"https://cdn.discordapp.com.evil.test/attachments/1/2/file.txt",
		"https://cdn.discordapp.com/not-attachments/1/2/file.txt",
		"https://cdn.discordapp.com/attachments/1/2/file.txt#fragment",
		"https://images-ext-1.discordapp.net/attachments/1/2/file.txt",
	} {
		require.Empty(t, AttachmentURL(raw), raw)
	}
}
