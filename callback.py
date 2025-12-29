import requests
import logging
import time

logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')

def callback_connection():
    max_retries = 3
    delay = 1  # Initial delay 1 second

    for attempt in range(max_retries + 1):  # Includes initial request + 3 retries
        logging.info(f"Attempt {attempt+1}/{max_retries}")

        try:
            response = requests.post(
                "http://files-service.os-framework/callback/connect",
                headers={"Content-Type": "application/json"},
                timeout=15
            )
            logging.info(f"Response status code: {response.status_code}")
            response.raise_for_status()
            break  # Break on success

        except requests.exceptions.Timeout:
            err_msg = "Request timeout (no response in 15 seconds)"
        except requests.exceptions.HTTPError as err:
            err_msg = f"HTTP error: {err.response.status_code} {err.response.reason}"
        except requests.exceptions.RequestException as err:
            err_msg = f"Network request exception: {err}"
        else:  # Exit directly if no exception
            return

        # Terminate after max retries
        if attempt == max_retries:
            logging.error(f"Final failure after {max_retries} retries: {err_msg}")
            return

        # Exponential backoff (1s→2s→4s)
        logging.error(f"Error occurred, retrying in {delay} seconds: {err_msg}")
        time.sleep(delay)
        delay *= 2  # Double delay for next retry

if __name__ == "__main__":
    callback_connection()