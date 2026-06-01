'use client';

import { useEffect, useState } from 'react';
import { useParams, useRouter } from 'next/navigation';
import Link from 'next/link';
import Image from 'next/image';
import { Game, GamePlayResult, WalletBalances } from '@/types';
import { api } from '@/lib/api';
import { useAuth } from '@/lib/auth-context';

const WIN_PROB: Record<string, number> = { low: 0.40, medium: 0.28, high: 0.15 };

/** Client-side simulation for demo mode (no real money, no backend call). */
function simulateDemo(game: Game, betAmount: number): GamePlayResult {
  const winProb = WIN_PROB[game.volatility] ?? 0.28;
  if (Math.random() >= winProb) {
    return { isWin: false, isJackpot: false, multiplier: 0, betAmount, winAmount: 0, currency: 'GC', balances: null };
  }
  const avgMultiplier = game.rtp / (100 * winProb);
  const multiplier = Math.max(1.0, +(avgMultiplier * (0.6 + Math.random() * 0.8)).toFixed(2));
  return {
    isWin: true, isJackpot: false, multiplier,
    betAmount, winAmount: +(betAmount * multiplier).toFixed(2),
    currency: 'GC', balances: null,
  };
}

export default function GameDetailPage() {
  const params = useParams();
  const router = useRouter();
  const { isAuthenticated, token, user, updateBalance } = useAuth();

  const [game, setGame] = useState<Game | null>(null);
  const [similarGames, setSimilarGames] = useState<Game[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState('');
  const [isFavorite, setIsFavorite] = useState(false);
  const [favoriteLoading, setFavoriteLoading] = useState(false);

  // ── Game-play panel state ──────────────────────────────────────────────────
  const [showPanel, setShowPanel] = useState(false);
  const [gameMode, setGameMode] = useState<'real' | 'demo'>('demo');
  const [currency, setCurrency] = useState<'GC' | 'SC'>('GC');
  const [betAmount, setBetAmount] = useState(0);
  const [isSpinning, setIsSpinning] = useState(false);
  const [lastResult, setLastResult] = useState<GamePlayResult | null>(null);
  const [walletBalances, setWalletBalances] = useState<WalletBalances | null>(null);

  useEffect(() => {
    const fetchGameData = async () => {
      try {
        const slug = params.slug as string;
        const gameData = await api.games.getBySlug(slug);
        if (gameData._id) {
          setGame(gameData);
          setBetAmount(gameData.minBet);

          if (isAuthenticated && token && user) {
            try {
              const profileData = await api.user.getProfile(token);
              setIsFavorite(profileData.favoriteGames?.includes(gameData._id) || false);
            } catch { /* ignore */ }
          }

          const allGames = await api.games.getAll({ category: gameData.category });
          setSimilarGames(allGames.filter((g: Game) => g._id !== gameData._id).slice(0, 4));
        } else {
          setError('Game not found');
        }
      } catch {
        setError('Failed to load game details');
      } finally {
        setIsLoading(false);
      }
    };
    fetchGameData();
  }, [params.slug, isAuthenticated, token, user]);

  // Fetch True wallet balances when panel opens in real mode
  useEffect(() => {
    if (showPanel && gameMode === 'real' && isAuthenticated && token) {
      api.wallet.getBalances(token)
        .then((r) => setWalletBalances(r?.balances ?? null))
        .catch(() => setWalletBalances(null));
    }
  }, [showPanel, gameMode, isAuthenticated, token]);

  const handlePlayGame = (mode: 'real' | 'demo') => {
    if (mode === 'real' && !isAuthenticated) {
      router.push('/login');
      return;
    }
    setGameMode(mode);
    setShowPanel(true);
    setLastResult(null);
  };

  const handleToggleFavorite = async () => {
    if (!isAuthenticated || !token) { router.push('/login'); return; }
    if (!game) return;
    setFavoriteLoading(true);
    try {
      await api.user.toggleFavorite(token, game._id);
      setIsFavorite(!isFavorite);
    } catch {
      alert('Failed to update favorites. Please try again.');
    } finally {
      setFavoriteLoading(false);
    }
  };

  const handleSpin = async () => {
    if (isSpinning || !game) return;

    if (gameMode === 'demo') {
      setIsSpinning(true);
      setLastResult(null);
      await new Promise((r) => setTimeout(r, 600)); // brief visual spin
      setLastResult(simulateDemo(game, betAmount));
      setIsSpinning(false);
      return;
    }

    // Real money — must be authenticated
    if (!isAuthenticated || !token) { router.push('/login'); return; }

    setIsSpinning(true);
    setLastResult(null);

    try {
      const result: GamePlayResult = await api.games.play(token, game.slug, betAmount, currency);

      if (result.message && !result.isWin) {
        alert(result.message);
      } else {
        setLastResult(result);
        if (result.balances) {
          setWalletBalances(result.balances);
          // Update the header balance display with GC value
          const gcNum = parseFloat(result.balances.gc);
          if (!isNaN(gcNum)) updateBalance(gcNum);
        }
      }
    } catch (err) {
      console.error('Spin error:', err);
    } finally {
      setIsSpinning(false);
    }
  };

  const gcBalance = walletBalances ? parseFloat(walletBalances.gc) : (user?.balance ?? 0);
  const scBalance = walletBalances
    ? parseFloat(walletBalances.sc_unplayed) + parseFloat(walletBalances.sc_redeemable)
    : (user?.bonusBalance ?? 0);
  const currentCurrencyBalance = currency === 'GC' ? gcBalance : scBalance;

  if (isLoading) {
    return (
      <div className="min-h-screen bg-gradient-to-br from-gray-900 via-purple-900 to-gray-900 flex items-center justify-center">
        <div className="text-white text-xl">Loading game...</div>
      </div>
    );
  }
  if (error || !game) {
    return (
      <div className="min-h-screen bg-gradient-to-br from-gray-900 via-purple-900 to-gray-900 flex items-center justify-center">
        <div className="text-center">
          <p className="text-red-400 text-xl mb-4">{error || 'Game not found'}</p>
          <Link href="/" className="text-yellow-400 hover:text-yellow-300">← Back to Home</Link>
        </div>
      </div>
    );
  }

  const volatilityColors = { low: 'text-green-400', medium: 'text-yellow-400', high: 'text-red-400' };

  return (
    <div className="min-h-screen bg-gradient-to-br from-gray-900 via-purple-900 to-gray-900 py-12 px-4">
      <div className="container mx-auto max-w-7xl">
        {/* Breadcrumb */}
        <div className="mb-6">
          <Link href="/" className="text-yellow-400 hover:text-yellow-300">Home</Link>
          <span className="text-gray-400 mx-2">/</span>
          <Link href="/games" className="text-yellow-400 hover:text-yellow-300">Games</Link>
          <span className="text-gray-400 mx-2">/</span>
          <span className="text-gray-300">{game.title}</span>
        </div>

        <div className="grid grid-cols-1 lg:grid-cols-3 gap-8">
          {/* Left: image + info + play panel */}
          <div className="lg:col-span-2">
            <div className="bg-gray-800/50 backdrop-blur-lg border border-purple-500/20 rounded-xl overflow-hidden">
              {/* Game image */}
              <div className="relative aspect-video bg-gray-700">
                <Image src={game.thumbnail} alt={game.title} fill className="object-cover" />
                {game.hasJackpot && game.jackpotAmount && (
                  <div className="absolute top-4 right-4 bg-yellow-500 text-gray-900 px-4 py-2 rounded-full font-bold">
                    💰 ${game.jackpotAmount.toLocaleString()}
                  </div>
                )}
                {game.isNew && (
                  <div className="absolute top-4 left-4 bg-purple-500 text-white px-3 py-1 rounded-full text-sm font-bold">NEW</div>
                )}
              </div>

              <div className="p-8">
                <h1 className="text-4xl font-bold text-white mb-4">{game.title}</h1>
                <div className="flex flex-wrap gap-4 mb-6">
                  <div className="flex items-center space-x-2">
                    <span className="text-gray-400">Provider:</span>
                    <span className="text-yellow-400 font-semibold">{game.provider}</span>
                  </div>
                  <div className="flex items-center space-x-2">
                    <span className="text-gray-400">Category:</span>
                    <span className="text-white font-semibold capitalize">{game.category.replace('-', ' ')}</span>
                  </div>
                </div>
                <p className="text-gray-300 mb-6 leading-relaxed">{game.description}</p>

                {/* Launch buttons */}
                <div className="flex flex-wrap gap-4">
                  <button
                    onClick={() => handlePlayGame('real')}
                    className="flex-1 min-w-[200px] py-4 px-6 bg-gradient-to-r from-yellow-400 to-yellow-600 text-gray-900 font-bold text-lg rounded-lg hover:from-yellow-500 hover:to-yellow-700 transition-all transform hover:scale-105"
                  >
                    Play Now 🎰
                  </button>
                  {game.demoAvailable && (
                    <button
                      onClick={() => handlePlayGame('demo')}
                      className="flex-1 min-w-[200px] py-4 px-6 bg-gray-700 text-white font-bold text-lg rounded-lg hover:bg-gray-600 transition-all"
                    >
                      Try Demo
                    </button>
                  )}
                  <button
                    onClick={handleToggleFavorite}
                    disabled={favoriteLoading}
                    className={`py-4 px-6 font-bold text-lg rounded-lg transition-all transform hover:scale-105 disabled:opacity-50 ${
                      isFavorite ? 'bg-red-500 hover:bg-red-600 text-white' : 'bg-gray-700 hover:bg-gray-600 text-white'
                    }`}
                    title={isFavorite ? 'Remove from favorites' : 'Add to favorites'}
                  >
                    {favoriteLoading ? '...' : isFavorite ? '❤️' : '🤍'}
                  </button>
                </div>

                {/* ── Game Play Panel ──────────────────────────────────────── */}
                {showPanel && (
                  <div className="mt-6 p-6 bg-gray-900/80 border border-yellow-500/20 rounded-xl">
                    {/* Mode / currency toggles */}
                    <div className="flex items-center justify-between mb-4 flex-wrap gap-2">
                      <div className="flex gap-2">
                        <button
                          onClick={() => { setGameMode('real'); setLastResult(null); }}
                          className={`px-4 py-2 rounded-lg font-semibold text-sm transition-all ${
                            gameMode === 'real' ? 'bg-yellow-500 text-gray-900' : 'bg-gray-700 text-white hover:bg-gray-600'
                          }`}
                        >
                          Real Money
                        </button>
                        <button
                          onClick={() => { setGameMode('demo'); setLastResult(null); }}
                          className={`px-4 py-2 rounded-lg font-semibold text-sm transition-all ${
                            gameMode === 'demo' ? 'bg-purple-500 text-white' : 'bg-gray-700 text-white hover:bg-gray-600'
                          }`}
                        >
                          Demo
                        </button>
                      </div>
                      {gameMode === 'real' && (
                        <div className="flex gap-2">
                          <button
                            onClick={() => { setCurrency('GC'); setLastResult(null); }}
                            className={`px-3 py-1 rounded text-sm font-semibold transition-all ${
                              currency === 'GC' ? 'bg-green-600 text-white' : 'bg-gray-700 text-gray-300 hover:bg-gray-600'
                            }`}
                          >
                            GC
                          </button>
                          <button
                            onClick={() => { setCurrency('SC'); setLastResult(null); }}
                            className={`px-3 py-1 rounded text-sm font-semibold transition-all ${
                              currency === 'SC' ? 'bg-blue-600 text-white' : 'bg-gray-700 text-gray-300 hover:bg-gray-600'
                            }`}
                          >
                            SC
                          </button>
                        </div>
                      )}
                    </div>

                    {/* Balance / mode info */}
                    {gameMode === 'real' ? (
                      <div className="mb-4 p-3 bg-green-500/10 border border-green-500/20 rounded-lg flex items-center justify-between">
                        <span className="text-gray-400 text-sm">
                          {currency === 'GC' ? 'GC Balance' : 'SC Balance'}
                        </span>
                        <span className={`font-bold text-lg ${currency === 'GC' ? 'text-green-400' : 'text-blue-400'}`}>
                          {currentCurrencyBalance.toFixed(4)} {currency}
                        </span>
                      </div>
                    ) : (
                      <div className="mb-4 p-3 bg-purple-500/10 border border-purple-500/20 rounded-lg">
                        <span className="text-purple-300 text-sm">Demo mode — no real funds involved</span>
                      </div>
                    )}

                    {/* Bet amount */}
                    <div className="mb-4">
                      <label className="text-gray-400 text-sm mb-2 block">Bet Amount</label>
                      <div className="flex items-center gap-2 mb-2">
                        <span className="text-gray-400 text-sm">$</span>
                        <input
                          type="number"
                          value={betAmount}
                          onChange={(e) => {
                            const v = parseFloat(e.target.value) || 0;
                            setBetAmount(Math.min(game.maxBet, Math.max(game.minBet, v)));
                          }}
                          min={game.minBet}
                          max={game.maxBet}
                          step={game.minBet}
                          className="w-28 px-3 py-2 bg-gray-700 border border-gray-600 rounded-lg text-white text-center font-bold focus:outline-none focus:border-yellow-500"
                        />
                      </div>
                      <div className="flex gap-2 flex-wrap">
                        <button onClick={() => setBetAmount(game.minBet)}
                          className="px-3 py-1 bg-gray-700 text-gray-300 rounded text-sm hover:bg-gray-600">Min</button>
                        {[1, 5, 10, 25].filter((v) => v >= game.minBet && v <= game.maxBet).map((v) => (
                          <button key={v} onClick={() => setBetAmount(v)}
                            className={`px-3 py-1 rounded text-sm transition-all ${
                              betAmount === v ? 'bg-yellow-500 text-gray-900 font-bold' : 'bg-gray-700 text-gray-300 hover:bg-gray-600'
                            }`}>
                            ${v}
                          </button>
                        ))}
                        <button onClick={() => setBetAmount(game.maxBet)}
                          className="px-3 py-1 bg-gray-700 text-gray-300 rounded text-sm hover:bg-gray-600">Max</button>
                      </div>
                    </div>

                    {/* Spin button */}
                    <button
                      onClick={handleSpin}
                      disabled={
                        isSpinning ||
                        (gameMode === 'real' && currentCurrencyBalance < betAmount)
                      }
                      className={`w-full py-4 rounded-xl font-bold text-xl transition-all ${
                        isSpinning
                          ? 'bg-gray-600 cursor-not-allowed text-gray-400'
                          : gameMode === 'real' && currentCurrencyBalance < betAmount
                          ? 'bg-gray-600 cursor-not-allowed text-gray-400'
                          : 'bg-gradient-to-r from-yellow-400 to-orange-500 text-gray-900 hover:from-yellow-500 hover:to-orange-600 transform hover:scale-[1.02]'
                      }`}
                    >
                      {isSpinning ? '🎰 Spinning...' : gameMode === 'demo' ? '🎮 Demo Spin' : `🎰 Spin — $${betAmount} ${currency}`}
                    </button>

                    {gameMode === 'real' && currentCurrencyBalance < betAmount && (
                      <p className="text-red-400 text-sm mt-2 text-center">
                        Insufficient {currency} balance.{' '}
                        <Link href="/deposit" className="text-yellow-400 hover:underline">Deposit funds →</Link>
                      </p>
                    )}

                    {/* Result display */}
                    {lastResult && (
                      <div className={`mt-4 p-5 rounded-xl text-center transition-all ${
                        lastResult.isWin
                          ? 'bg-green-500/15 border border-green-500/30'
                          : 'bg-red-500/10 border border-red-500/20'
                      }`}>
                        {lastResult.isWin ? (
                          <>
                            <div className="text-5xl mb-2">{lastResult.isJackpot ? '🏆' : '🎉'}</div>
                            <div className="text-green-400 font-bold text-2xl">
                              {lastResult.isJackpot ? 'JACKPOT!' : 'YOU WON!'}
                            </div>
                            <div className="text-white text-3xl font-bold mt-1">
                              +${lastResult.winAmount.toFixed(2)}
                            </div>
                            <div className="text-gray-400 text-sm mt-1">
                              {lastResult.multiplier}× multiplier
                            </div>
                            {lastResult.balances && gameMode === 'real' && (
                              <div className="mt-3 pt-3 border-t border-green-500/20 grid grid-cols-3 gap-2 text-xs">
                                <div>
                                  <div className="text-gray-400">GC</div>
                                  <div className="text-green-300 font-semibold">{parseFloat(lastResult.balances.gc).toFixed(2)}</div>
                                </div>
                                <div>
                                  <div className="text-gray-400">SC Unplayed</div>
                                  <div className="text-blue-300 font-semibold">{parseFloat(lastResult.balances.sc_unplayed).toFixed(2)}</div>
                                </div>
                                <div>
                                  <div className="text-gray-400">SC Earned</div>
                                  <div className="text-purple-300 font-semibold">{parseFloat(lastResult.balances.sc_redeemable).toFixed(2)}</div>
                                </div>
                              </div>
                            )}
                          </>
                        ) : (
                          <>
                            <div className="text-4xl mb-2">😔</div>
                            <div className="text-red-400 font-bold text-xl">No luck this time</div>
                            <div className="text-gray-400 text-sm mt-1">Try again for a chance to win!</div>
                          </>
                        )}
                      </div>
                    )}
                  </div>
                )}
              </div>
            </div>

            {/* Similar games */}
            {similarGames.length > 0 && (
              <div className="mt-8">
                <h2 className="text-2xl font-bold text-white mb-6">Similar Games</h2>
                <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
                  {similarGames.map((sg) => (
                    <Link key={sg._id} href={`/games/${sg.slug}`}
                      className="group bg-gray-800/50 rounded-lg overflow-hidden hover:ring-2 hover:ring-yellow-400 transition-all">
                      <div className="relative aspect-square bg-gray-700">
                        <Image src={sg.thumbnail} alt={sg.title} fill className="object-cover" />
                      </div>
                      <div className="p-3">
                        <h3 className="text-white font-semibold text-sm truncate">{sg.title}</h3>
                        <p className="text-gray-400 text-xs">{sg.provider}</p>
                      </div>
                    </Link>
                  ))}
                </div>
              </div>
            )}
          </div>

          {/* Right sidebar */}
          <div className="space-y-6">
            {/* Game Stats */}
            <div className="bg-gray-800/50 backdrop-blur-lg border border-purple-500/20 rounded-xl p-6">
              <h3 className="text-white font-bold text-lg mb-4">Game Details</h3>
              <div className="space-y-4">
                <div className="flex justify-between items-center">
                  <span className="text-gray-400">RTP</span>
                  <span className="text-white font-semibold">{game.rtp}%</span>
                </div>
                <div className="flex justify-between items-center">
                  <span className="text-gray-400">Volatility</span>
                  <span className={`font-semibold capitalize ${volatilityColors[game.volatility]}`}>{game.volatility}</span>
                </div>
                <div className="flex justify-between items-center">
                  <span className="text-gray-400">Min Bet</span>
                  <span className="text-white font-semibold">${game.minBet}</span>
                </div>
                <div className="flex justify-between items-center">
                  <span className="text-gray-400">Max Bet</span>
                  <span className="text-white font-semibold">${game.maxBet}</span>
                </div>
              </div>
            </div>

            {/* True wallet breakdown (shown when playing for real) */}
            {isAuthenticated && walletBalances && (
              <div className="bg-gray-800/50 backdrop-blur-lg border border-purple-500/20 rounded-xl p-6">
                <h3 className="text-white font-bold text-lg mb-4">Wallet</h3>
                <div className="space-y-3">
                  <div className="flex justify-between items-center">
                    <span className="text-gray-400 text-sm">🟡 Gold Coins (GC)</span>
                    <span className="text-green-400 font-semibold">{parseFloat(walletBalances.gc).toFixed(2)}</span>
                  </div>
                  <div className="flex justify-between items-center">
                    <span className="text-gray-400 text-sm">🔵 SC (unplayed)</span>
                    <span className="text-blue-400 font-semibold">{parseFloat(walletBalances.sc_unplayed).toFixed(2)}</span>
                  </div>
                  <div className="flex justify-between items-center">
                    <span className="text-gray-400 text-sm">🟣 SC (redeemable)</span>
                    <span className="text-purple-400 font-semibold">{parseFloat(walletBalances.sc_redeemable).toFixed(2)}</span>
                  </div>
                </div>
              </div>
            )}

            {/* Features */}
            {game.features.length > 0 && (
              <div className="bg-gray-800/50 backdrop-blur-lg border border-purple-500/20 rounded-xl p-6">
                <h3 className="text-white font-bold text-lg mb-4">Features</h3>
                <div className="flex flex-wrap gap-2">
                  {game.features.map((feature, i) => (
                    <span key={i} className="px-3 py-1 bg-purple-500/20 text-purple-300 rounded-full text-sm">
                      {feature}
                    </span>
                  ))}
                </div>
              </div>
            )}

            <div className="bg-gray-800/50 backdrop-blur-lg border border-purple-500/20 rounded-xl p-6">
              <h3 className="text-white font-bold text-lg mb-4">Why Play This Game?</h3>
              <ul className="space-y-3 text-sm text-gray-300">
                <li className="flex items-start"><span className="text-yellow-400 mr-2">✓</span>Certified fair gaming</li>
                <li className="flex items-start"><span className="text-yellow-400 mr-2">✓</span>Mobile friendly</li>
                <li className="flex items-start"><span className="text-yellow-400 mr-2">✓</span>Fast payouts</li>
                <li className="flex items-start"><span className="text-yellow-400 mr-2">✓</span>Instant play — no download</li>
              </ul>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
