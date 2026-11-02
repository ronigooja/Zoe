package voice

func RotateRequired(contextChars, budget int) bool { return budget > 0 && contextChars > budget }
