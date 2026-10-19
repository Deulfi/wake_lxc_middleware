package middleware

// startingPageHTML is the page served while a container is starting. It
// polls /__wake__/status every 2s and reloads the page once ready.
const startingPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Service Starting</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            min-height: 100vh;
            display: flex;
            justify-content: center;
            align-items: center;
            text-align: center;
            padding: 20px;
        }
        .container {
            background: rgba(255, 255, 255, 0.1);
            backdrop-filter: blur(10px);
            border-radius: 16px;
            padding: 32px 28px;
            box-shadow: 0 8px 32px rgba(31, 38, 135, 0.37);
            border: 1px solid rgba(255, 255, 255, 0.18);
            max-width: 420px;
            width: 90%;
        }
        .logo { font-size: 3rem; margin-bottom: 12px; animation: pulse 2s ease-in-out infinite alternate; }
        @keyframes pulse { from { transform: scale(1); } to { transform: scale(1.08); } }
        h1 { font-size: 1.5rem; margin-bottom: 12px; font-weight: 600; }
        .spinner {
            border: 3px solid rgba(255, 255, 255, 0.3);
            border-radius: 50%;
            border-top: 3px solid white;
            width: 40px; height: 40px;
            animation: spin 1s linear infinite;
            margin: 20px auto;
        }
        @keyframes spin { 0% { transform: rotate(0deg); } 100% { transform: rotate(360deg); } }
        .status-text { font-size: 1.05rem; line-height: 1.4; opacity: 0.95; margin: 16px 0; min-height: 28px; }
        .timer { font-size: 0.85rem; opacity: 0.7; margin: 8px 0; }
        .error { background: rgba(255, 107, 107, 0.2); border: 1px solid rgba(255, 107, 107, 0.3); }
    </style>
</head>
<body>
    <div class="container" id="main-container">
        <div class="logo">&#128680;</div>
        <h1>Starting...</h1>
        <div class="spinner"></div>
        <div class="status-text" id="status-text">Checking status...</div>
        <div class="timer" id="timer">Elapsed: 0s</div>
    </div>
    <script>
        var startedAt = Date.now();
        var statusText = document.getElementById('status-text');
        var timerEl = document.getElementById('timer');
        var container = document.getElementById('main-container');

        setInterval(function() {
            timerEl.textContent = "Elapsed: " + Math.floor((Date.now() - startedAt) / 1000) + "s";
        }, 1000);

        function pollStatus() {
            fetch('/__wake__/status', { cache: 'no-store' })
                .then(function(r) { return r.json(); })
                .then(function(data) {
                    if (data.message) statusText.textContent = data.message;
                    if (data.level === 'error') container.classList.add('error');
                    else container.classList.remove('error');
                    if (data.ready) { location.reload(); return; }
                })
                .catch(function() {
                    statusText.textContent = 'Connection lost, retrying...';
                })
                .finally(function() {
                    setTimeout(pollStatus, 2000);
                });
        }
        pollStatus();
    </script>
</body>
</html>`
