module.exports = {
  apps: [
    {
      name: "cross-arb",
      script: "./cross-arb",
      args: "--maker DECIBEL --taker LIGHTER --symbol BTC --qty 0.01 --z-open 2.0 --z-close 0.5 --z-stop -1.5 --window 300 --max-hold 5m --cooldown 3s --live-validate --max-rounds 3",
      cwd: "/home/ubuntu/cross-arb",
      interpreter: "none",
      autorestart: true,
      max_restarts: 10,
      restart_delay: 5000,
      watch: false,
      env: {},
    },
  ],
};
