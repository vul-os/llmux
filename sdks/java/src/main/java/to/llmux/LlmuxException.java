package to.llmux;

/** Thrown when the llmux sidecar cannot be located, started, or made healthy. */
public class LlmuxException extends RuntimeException {

    public LlmuxException(String message) {
        super(message);
    }

    public LlmuxException(String message, Throwable cause) {
        super(message, cause);
    }
}
